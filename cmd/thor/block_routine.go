package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"time"

	"github.com/vechain/thor/co"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/cmd/thor/minheap"
	"github.com/vechain/thor/comm"
	Consensus "github.com/vechain/thor/consensus"
	Logdb "github.com/vechain/thor/logdb"
	Packer "github.com/vechain/thor/packer"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/tx"
	Txpool "github.com/vechain/thor/txpool"
)

func produceBlock(ctx context.Context, component *component, logdb *Logdb.LogDB) {
	var goes co.Goes

	packedChan := make(chan *packedEvent)
	bestBlockUpdated := make(chan *block.Block, 1)

	goes.Go(func() { consentLoop(ctx, component, logdb, bestBlockUpdated, packedChan) })
	goes.Go(func() { packLoop(ctx, component, bestBlockUpdated, packedChan) })

	log.Info("Block consensus started")
	log.Info("Block packer started")
	goes.Wait()
	log.Info("Block consensus stoped")
	log.Info("Block packer stoped")
}

type orphan struct {
	blk       *block.Block
	timestamp uint64 // 块成为 orpahn 的时间, 最多维持 5 分钟
}

type newBlockEvent struct {
	Blk      *block.Block
	Receipts tx.Receipts
	Trunk    bool
	IsSynced bool
}

type packedEvent struct {
	blk      *block.Block
	receipts tx.Receipts
	ack      chan struct{}
}

func consentLoop(
	ctx context.Context,
	component *component,
	logdb *Logdb.LogDB,
	bestBlockUpdated chan *block.Block,
	packedChan chan *packedEvent,
) {
	futures := minheap.NewBlockMinHeap()
	orphanMap := make(map[thor.Bytes32]*orphan)
	updateChainFn := func(newBlk *newBlockEvent) error {
		return updateChain(ctx, component, logdb, newBlk, bestBlockUpdated)
	}
	consentFn := func(blk *block.Block, isSynced bool) error {
		trunk, receipts, err := component.consensus.Consent(blk, uint64(time.Now().Unix()))
		if err != nil {
			//log.Warn(fmt.Sprintf("received new block(#%v bad)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size(), "err", err.Error())
			if Consensus.IsFutureBlock(err) {
				futures.Push(blk)
			} else if Consensus.IsParentNotFound(err) {
				parentID := blk.Header().ParentID()
				if _, ok := orphanMap[parentID]; !ok {
					orphanMap[parentID] = &orphan{blk: blk, timestamp: uint64(time.Now().Unix())}
				}
			}
			return err
		}

		return updateChainFn(&newBlockEvent{
			Blk:      blk,
			Trunk:    trunk,
			Receipts: receipts,
			IsSynced: isSynced,
		})
	}

	subChan := make(chan *comm.NewBlockEvent, 100)
	sub := component.communicator.SubscribeBlock(subChan)

	ticker := time.NewTicker(time.Duration(thor.BlockInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			sub.Unsubscribe()
			return
		case <-ticker.C:
			if blk := futures.Pop(); blk != nil {
				consentFn(blk, false)
			}
		case ev := <-subChan:
			if err := consentFn(ev.Block, ev.IsSynced); err != nil {
				break
			}

			if orphan, ok := orphanMap[ev.Block.Header().ID()]; ok {
				if orphan.timestamp+300 >= uint64(time.Now().Unix()) {
					if err := consentFn(orphan.blk, false); err != nil && Consensus.IsParentNotFound(err) {
						break
					}
				}
				delete(orphanMap, ev.Block.Header().ID())
			}
		case packed := <-packedChan:
			if trunk, err := component.consensus.IsTrunk(packed.blk.Header()); err == nil {
				updateChainFn(&newBlockEvent{
					Blk:      packed.blk,
					Trunk:    trunk,
					Receipts: packed.receipts,
					IsSynced: false,
				})
				packed.ack <- struct{}{}
			}
		}
	}
}

func packLoop(
	ctx context.Context,
	component *component,
	bestBlockUpdated chan *block.Block,
	packedChan chan *packedEvent,
) {
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()

	if !component.communicator.IsSynced() {
		log.Warn("Chain data has not synced, waiting...")
	}
	for !component.communicator.IsSynced() {
		select {
		case <-ctx.Done():
			return
		default:
			time.Sleep(1 * time.Second)
		}
	}
	log.Info("Chain data has synced")

	var (
		ts        uint64
		adopt     Packer.Adopt
		commit    Packer.Commit
		bestBlock *block.Block
		err       error
	)

	bestBlock, err = component.chain.GetBestBlock()
	if err != nil {
		log.Error(fmt.Sprintf("%v", err))
		return
	}
	sendBestBlock(bestBlockUpdated, bestBlock)

	for {
		timer.Reset(2 * time.Second)

		select {
		case <-ctx.Done():
			return
		case bestBlock = <-bestBlockUpdated:
			ts, adopt, commit, err = component.packer.Prepare(bestBlock.Header(), uint64(time.Now().Unix()))
			if err != nil {
				log.Error(fmt.Sprintf("%v", err))
				break
			}
		case <-timer.C:
			now := uint64(time.Now().Unix())
			if now >= ts && now < ts+thor.BlockInterval {
				ts = 0
				pack(component.txpool, component.packer, adopt, commit, component.privateKey, packedChan)
			} else if ts > now {
				//fmt.Printf("after %v seconds to pack.\r\n", ts-now)
			}
		}
	}
}

func pack(
	txpool *Txpool.TxPool,
	packer *Packer.Packer,
	adopt Packer.Adopt,
	commit Packer.Commit,
	privateKey *ecdsa.PrivateKey,
	packedChan chan *packedEvent,
) {
	adoptTx := func() {
		for _, tx := range txpool.Pending() {
			err := adopt(tx)
			switch {
			case Packer.IsBadTx(err) || Packer.IsKnownTx(err):
				txpool.Remove(tx.ID())
			case Packer.IsGasLimitReached(err):
				return
			default:
			}
		}
	}

	startTime := mclock.Now()
	adoptTx()
	blk, receipts, err := commit(privateKey)
	if err != nil {
		log.Error(fmt.Sprintf("%v", err))
		return
	}
	elapsed := mclock.Now() - startTime

	if elapsed > 0 {
		gasUsed := blk.Header().GasUsed()
		// calc target gas limit only if gas used above third of gas limit
		if gasUsed > blk.Header().GasLimit()/3 {
			targetGasLimit := uint64(thor.TolerableBlockPackingTime) * gasUsed / uint64(elapsed)
			packer.SetTargetGasLimit(targetGasLimit)
		}
	}

	//log.Info(fmt.Sprintf("proposed new block(#%v)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size())
	pe := &packedEvent{
		blk:      blk,
		receipts: receipts,
		ack:      make(chan struct{}),
	}
	packedChan <- pe
	<-pe.ack
}

func updateChain(
	ctx context.Context,
	component *component,
	logdb *Logdb.LogDB,
	newBlk *newBlockEvent,
	bestBlockUpdated chan *block.Block,
) error {
	fork, err := component.chain.AddBlock(newBlk.Blk, newBlk.Receipts, newBlk.Trunk)
	if err != nil {
		log.Error(fmt.Sprintf("%v", err))
		return err
	}

	if newBlk.Trunk {
		if !newBlk.IsSynced {
			header := newBlk.Blk.Header()
			if signer, err := header.Signer(); err == nil {
				log.Info("Best block updated",
					"number", header.Number(),
					"id", header.ID().AbbrevString(),
					"total-score", header.TotalScore(),
					"proposer", signer.String(),
				)
			}
		}

		sendBestBlock(bestBlockUpdated, newBlk.Blk)
		component.communicator.BroadcastBlock(newBlk.Blk)

		// fork
		logs := []*Logdb.Log{}
		var index uint32
		txs := newBlk.Blk.Transactions()
		for i, receipt := range newBlk.Receipts {
			for _, output := range receipt.Outputs {
				tx := txs[i]
				signer, err := tx.Signer()
				if err != nil {
					log.Error(fmt.Sprintf("%v", err))
					return err
				}
				header := newBlk.Blk.Header()
				for _, log := range output.Logs {
					logs = append(logs, Logdb.NewLog(header, index, tx.ID(), signer, log))
				}
				index++
			}
		}
		forkIDs := make([]thor.Bytes32, len(fork.Branch), len(fork.Branch))
		for i, blk := range fork.Branch {
			forkIDs[i] = blk.Header().ID()
			for _, tx := range blk.Transactions() {
				component.txpool.Add(tx)
			}
		}
		logdb.Insert(logs, forkIDs)
	}

	return nil
}

func sendBestBlock(bestBlockUpdated chan *block.Block, blk *block.Block) {
	for {
		select {
		case bestBlockUpdated <- blk:
			return
		case <-bestBlockUpdated:
		}
	}
}
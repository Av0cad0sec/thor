package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/vechain/thor/api/utils/httpx"
	"github.com/vechain/thor/cry"
)

//BlockHTTPPathPrefix http path prefix
const BlockHTTPPathPrefix = "/block"

//NewBlockHTTPRouter add path to router
func NewBlockHTTPRouter(router *mux.Router, bi *BlockInterface) {
	sub := router.PathPrefix(BlockHTTPPathPrefix).Subrouter()
	sub.Path("/hash/{hash}").Methods("GET").HandlerFunc(httpx.WrapHandlerFunc(bi.handleGetBlockByHash))
	sub.Path("/number/{number}").Methods("GET").HandlerFunc(httpx.WrapHandlerFunc(bi.handleGetBlockByNumber))
}
func (bi *BlockInterface) handleGetBlockByHash(w http.ResponseWriter, req *http.Request) error {
	query := mux.Vars(req)
	if query == nil {
		return httpx.Error(errors.New(" No Params! "), 400)
	}
	hashstring, ok := query["hash"]
	if !ok {
		return httpx.Error(errors.New(" Invalid Params! "), 400)
	}
	hash, err := cry.ParseHash(hashstring)
	if err != nil {
		return httpx.Error(errors.New(" Parse block hash failed! "), 400)
	}
	block, err := bi.GetBlockByHash(*hash)
	if err != nil {
		return httpx.Error(errors.New(" Get block failed! "), 400)
	}
	str, err := json.Marshal(block)
	if err != nil {
		return httpx.Error(errors.New(" System Error! "), 400)
	}
	w.Write(str)
	return nil
}

func (bi *BlockInterface) handleGetBlockByNumber(w http.ResponseWriter, req *http.Request) error {
	query := mux.Vars(req)
	if query == nil {
		return httpx.Error(errors.New(" No Params! "), 400)
	}
	number, ok := query["number"]
	if !ok {
		return httpx.Error(errors.New(" Invalid Params! "), 400)
	}
	bn, err := strconv.Atoi(number)
	if err != nil {
		return httpx.Error(errors.New(" Parse block hash failed! "), 400)
	}
	block, err := bi.GetBlockByNumber(uint32(bn))
	if err != nil {
		return httpx.Error(errors.New(" Get block failed! "), 400)
	}
	str, err := json.Marshal(block)
	if err != nil {
		return httpx.Error(errors.New(" System Error! "), 400)
	}
	w.Write(str)
	return nil
}

package opsimulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	ophttp "github.com/ethereum-optimism/optimism/op-service/httputil"
	"github.com/ethereum-optimism/optimism/op-service/tasks"

	"github.com/ethereum-optimism/supersim/config"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	host = "127.0.0.1"
)

type OpSimulator struct {
	config.Chain // the chain that op-sim is wrapping

	log log.Logger

	l1Chain config.Chain

	// Long running tasks
	bgTasks       tasks.Group
	bgTasksCtx    context.Context
	bgTasksCancel context.CancelFunc
	peers         map[uint64]config.Chain

	// One time tasks at startup
	startupTasks       tasks.Group
	startupTasksCtx    context.Context
	startupTasksCancel context.CancelFunc

	port       uint64
	httpServer *ophttp.HTTPServer

	stopped atomic.Bool
}

// OpSimulator wraps around the l2 chain. By embedding `Chain`, it also implements the same inteface
func New(log log.Logger, port uint64, l1Chain, l2Chain config.Chain, peers map[uint64]config.Chain) *OpSimulator {
	bgTasksCtx, bgTasksCancel := context.WithCancel(context.Background())
	startupTasksCtx, startupTasksCancel := context.WithCancel(context.Background())

	return &OpSimulator{
		Chain: l2Chain,

		log:     log,
		port:    port,
		l1Chain: l1Chain,

		bgTasksCtx:    bgTasksCtx,
		bgTasksCancel: bgTasksCancel,
		bgTasks: tasks.Group{
			HandleCrit: func(err error) {
				log.Error("opsim bg task failed", "err", err)
				panic(err)
			},
		},

		startupTasksCtx:    startupTasksCtx,
		startupTasksCancel: startupTasksCancel,
		startupTasks: tasks.Group{
			HandleCrit: func(err error) {
				log.Error("opsim startup task failed", err)
				panic(err)
			},
		},

		peers: peers,
	}
}

func (opSim *OpSimulator) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/", corsHandler(opSim.handler(ctx)))

	hs, err := ophttp.StartHTTPServer(net.JoinHostPort(host, fmt.Sprintf("%d", opSim.port)), mux)
	if err != nil {
		return fmt.Errorf("failed to start HTTP RPC server: %w", err)
	}

	cfg := opSim.Config()
	opSim.log.Debug("started opsimulator", "name", cfg.Name, "chain.id", cfg.ChainID, "addr", hs.Addr())
	opSim.httpServer = hs

	if opSim.port == 0 {
		opSim.port, err = strconv.ParseUint(strings.Split(hs.Addr().String(), ":")[1], 10, 64)
		if err != nil {
			panic(fmt.Errorf("unexpected opsimulator listening port: %w", err))
		}
	}

	opSim.startStartupTasks(ctx)
	if err := opSim.startupTasks.Wait(); err != nil {
		return fmt.Errorf("failed to start opsimulator: %w", err)
	}

	opSim.startBackgroundTasks()
	return nil
}

func (opSim *OpSimulator) Stop(ctx context.Context) error {
	if opSim.stopped.Load() {
		return errors.New("already stopped")
	}
	if !opSim.stopped.CompareAndSwap(false, true) {
		return nil // someone else stopped
	}

	opSim.bgTasksCancel()
	return opSim.httpServer.Stop(ctx)
}

func (opSim *OpSimulator) startStartupTasks(ctx context.Context) {
	opSim.startupTasks.Go(func() error {
		if opSim.Config().ForkConfig != nil && opSim.Config().ForkConfig.UseInterop {
			if err := configureInteropForChain(ctx, opSim.Chain); err != nil {
				return fmt.Errorf("failed to configure interop: %w", err)
			}
		}

		for _, chainID := range opSim.Config().L2Config.DependencySet {
			if err := opSim.addDependency(chainID); err != nil {
				return fmt.Errorf("failed to configure dependency set: %w", err)
			}
		}

		return nil
	})
}

func (opSim *OpSimulator) startBackgroundTasks() {
	// Relay deposit tx from L1 to L2
	opSim.bgTasks.Go(func() error {
		depositTxCh := make(chan *types.DepositTx)
		portalAddress := common.Address(opSim.Config().L2Config.L1Addresses.OptimismPortalProxy)
		sub, err := SubscribeDepositTx(context.Background(), opSim.l1Chain.EthClient(), portalAddress, depositTxCh)
		if err != nil {
			return fmt.Errorf("failed to subscribe to deposit tx: %w", err)
		}

		for {
			select {
			case dep := <-depositTxCh:
				depTx := types.NewTx(dep)
				opSim.log.Debug("received deposit tx", "hash", depTx.Hash().String())

				clnt := opSim.EthClient()
				if err := clnt.SendTransaction(opSim.bgTasksCtx, depTx); err != nil {
					opSim.log.Error("failed to submit deposit tx: %w", err)
				}

				opSim.log.Debug("submitted deposit tx", "hash", depTx.Hash().String())

			case <-opSim.bgTasksCtx.Done():
				sub.Unsubscribe()
				close(depositTxCh)
			}
		}
	})
}

func (opSim *OpSimulator) handler(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// setup an intermediate buffer so the request body is inspectable
		var buf bytes.Buffer
		body := io.TeeReader(r.Body, &buf)
		r.Body = io.NopCloser(&buf)

		// decode the fields we're interested in inspecting
		msgs, isBatchRequest, err := readJsonMessages(body)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse JSON-RPC request: %s", err), http.StatusBadRequest)
			return
		}

		rpcClient := opSim.EthClient().Client()
		batchRes := make([]jsonRpcMessage, len(msgs))
		for i, msg := range msgs {
			if msg.Method == "eth_sendRawTransaction" {
				var params []hexutil.Bytes
				if err := json.Unmarshal(msg.Params, &params); err != nil {
					opSim.log.Error(fmt.Sprintf("bad params sent to eth_sendRawTransaction: %s", err))
					batchRes[i] = jsonRpcMessage{
						Version: vsn,
						Error: &jsonError{
							Code:    ParseErr,
							Message: err.Error(),
						},
						ID: msg.ID,
					}
					continue
				}
				if len(params) != 1 {
					opSim.log.Error("eth_sendRawTransaction request has invalid number of params")
					batchRes[i] = jsonRpcMessage{
						Version: vsn,
						Error: &jsonError{
							Code:    InvalidParams,
							Message: "eth_sendRawTransaction request has invalid number of params",
						},
						ID: msg.ID,
					}
					continue
				}

				tx := new(types.Transaction)
				if err := tx.UnmarshalBinary(params[0]); err != nil {
					opSim.log.Error(fmt.Sprintf("failed to decode transaction data: %s", err))
					batchRes[i] = jsonRpcMessage{
						Version: vsn,
						Error: &jsonError{
							Code:    InvalidParams,
							Message: err.Error(),
						},
						ID: msg.ID,
					}
					continue
				}

				if err := opSim.checkInteropInvariants(ctx, tx); err != nil {
					opSim.log.Error(fmt.Sprintf("interop invariants not met: %s", err))
					batchRes[i] = jsonRpcMessage{
						Version: vsn,
						Error: &jsonError{
							Code:    InvalidParams,
							Message: err.Error(),
						},
						ID: msg.ID,
					}
					continue
				}
			}

			var jsonErr *jsonError
			batchRes[i], jsonErr = forwardRPCRequest(ctx, rpcClient, msg)
			if jsonErr != nil {
				opSim.log.Error(fmt.Sprintf("RPC returned an error: %s", jsonErr))
				batchRes[i] = jsonRpcMessage{
					Version: vsn,
					Error:   jsonErr,
					ID:      msg.ID,
				}
			}
		}

		if isBatchRequest {
			if err := json.NewEncoder(w).Encode(batchRes); err != nil {
				opSim.log.Error(fmt.Sprintf("failed to write batch response: %s", err))
				http.Error(w, "Failed to write batch response", http.StatusInternalServerError)
				return
			}
		} else {
			if err := json.NewEncoder(w).Encode(batchRes[0]); err != nil {
				opSim.log.Error(fmt.Sprintf("failed to write response: %s", err))
				http.Error(w, "Failed to write batch response", http.StatusInternalServerError)
				return
			}
		}
	}
}

// Forward a JSON-RPC request to the Geth RPC server
func forwardRPCRequest(ctx context.Context, rpcClient *rpc.Client, req *jsonRpcMessage) (jsonRpcMessage, *jsonError) {
	var result json.RawMessage
	var params []interface{}

	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return jsonRpcMessage{}, &jsonError{
				Code:    InvalidParams,
				Message: err.Error(),
			}
		}
	}

	if err := rpcClient.CallContext(ctx, &result, req.Method, params...); err != nil {
		if je, ok := err.(*jsonError); ok {
			return jsonRpcMessage{}, je
		}

		return jsonRpcMessage{}, errorMessage(err).Error
	}

	return jsonRpcMessage{
		Version: vsn,
		Result:  result,
		ID:      req.ID,
	}, nil
}

// Update dependency set on the L2#L1BlockInterop using a deposit tx
func (opSim *OpSimulator) addDependency(chainID uint64) error {
	dep, err := NewAddDependencyDepositTx(big.NewInt(int64(chainID)))
	if err != nil {
		return fmt.Errorf("failed to create setConfig deposit tx: %w", err)
	}

	tx, clnt := types.NewTx(dep), opSim.EthClient()
	if err := clnt.SendTransaction(opSim.startupTasksCtx, tx); err != nil {
		return fmt.Errorf("failed to send setConfig deposit tx: %w", err)
	}

	txReceipt, err := bind.WaitMined(opSim.startupTasksCtx, clnt, tx)
	if err != nil {
		return fmt.Errorf("failed to get tx receipt for deposit tx: %w", err)
	}
	if txReceipt.Status == 0 {
		return fmt.Errorf("setConfig deposit tx failed")
	}

	return nil
}

func (opSim *OpSimulator) checkInteropInvariants(ctx context.Context, tx *types.Transaction) error {
	logs, err := opSim.SimulatedLogs(ctx, tx)
	if err != nil {
		return fmt.Errorf("failed to simulate transaction: %w", err)
	}

	crossL2Inbox := NewCrossL2Inbox()
	var executingMessages []executingMessage
	for _, log := range logs {
		executingMessage, err := crossL2Inbox.decodeExecutingMessageLog(&log)
		if err != nil {
			return fmt.Errorf("failed to decode executing messages from transaction logs: %w", err)
		}

		if executingMessage != nil {
			executingMessages = append(executingMessages, *executingMessage)
		}
	}

	if len(executingMessages) >= 1 {
		for _, executingMessage := range executingMessages {
			identifier := executingMessage.Identifier
			sourceChain, ok := opSim.peers[identifier.ChainId.Uint64()]
			if !ok {
				return fmt.Errorf("no chain found for chain id: %d", identifier.ChainId)
			}

			sourceClient := sourceChain.EthClient()
			identifierBlock, err := sourceClient.BlockByNumber(ctx, identifier.BlockNumber)
			if err != nil {
				return fmt.Errorf("failed to fetch executing message block: %w", err)
			}

			if identifier.Timestamp.Cmp(new(big.Int).SetUint64(identifierBlock.Time())) != 0 {
				return fmt.Errorf("executing message identifier does not match block timestamp: %w", err)
			}

			filterQuery := ethereum.FilterQuery{Addresses: []common.Address{identifier.Origin}, FromBlock: identifier.BlockNumber, ToBlock: identifier.BlockNumber}
			logs, err := sourceClient.FilterLogs(ctx, filterQuery)
			if err != nil {
				return fmt.Errorf("failed to fetch initiating message logs: %w", err)
			}

			var initiatingMessageLogs []types.Log
			for _, log := range logs {
				logIndex := big.NewInt(int64(log.Index))
				if logIndex.Cmp(identifier.LogIndex) == 0 {
					initiatingMessageLogs = append(initiatingMessageLogs, log)
				}
			}

			if len(initiatingMessageLogs) == 0 {
				return fmt.Errorf("initiating message not found")
			}

			// Since we look for a log at a specific index, this should never be more than 1.
			if len(initiatingMessageLogs) > 1 {
				return fmt.Errorf("unexpected number of initiating messages found: %d", len(initiatingMessageLogs))
			}

			initiatingMsgPayloadHash := crypto.Keccak256Hash(messagePayloadBytes(&initiatingMessageLogs[0]))
			if common.BytesToHash(executingMessage.MsgHash[:]).Cmp(initiatingMsgPayloadHash) != 0 {
				return fmt.Errorf("executing and initiating message fields are not equal")
			}
		}
	}

	return nil
}

func messagePayloadBytes(log *types.Log) []byte {
	msg := []byte{}
	for _, topic := range log.Topics {
		msg = append(msg, topic.Bytes()...)
	}
	return append(msg, log.Data...)
}

// Overridden such that the port field can appropiately be set
func (opSim *OpSimulator) Config() *config.ChainConfig {
	// we dereference the config so that a copy is made.
	//  - NOTE: This is only okay since the field were modifying are non-pointer fields
	cfg := *opSim.Chain.Config()
	cfg.Port = opSim.port
	return &cfg
}

// Overridden such that the correct port is used
func (opSim *OpSimulator) Endpoint() string {
	return fmt.Sprintf("http://%s:%d", host, opSim.port)
}

// Overridden such that the right endpoint is used
func (opSim *OpSimulator) String() string {
	var b strings.Builder
	cfg := opSim.Config()
	fmt.Fprintf(&b, "Name: %s    Chain ID: %d    RPC: %s    LogPath: %s", cfg.Name, cfg.ChainID, opSim.Endpoint(), opSim.LogPath())
	return b.String()
}

func corsHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

//
// Copyright 2021-2022, Offchain Labs, Inc. All rights reserved.
//

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	flag "github.com/spf13/pflag"

	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/statetransfer"
	"github.com/offchainlabs/nitro/util"
	"github.com/offchainlabs/nitro/validator"
)

func printSampleUsage() {
	progname := os.Args[0]
	fmt.Printf("\n")
	fmt.Printf("Sample usage:                  %s --help \n", progname)
}

func main() {
	ctx := context.Background()

	nodeConfig, l1wallet, l2wallet, err := ParseNode(ctx)
	if err != nil {
		printSampleUsage()
		if !strings.Contains(err.Error(), "help requested") {
			fmt.Printf("%s\n", err.Error())
		}

		return
	}
	glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.TerminalFormat(false)))
	glogger.Verbosity(log.Lvl(nodeConfig.LogLevel))
	log.Root().SetHandler(glogger)

	log.Info("Running Arbitrum nitro node")

	if nodeConfig.Node.NoL1Listener {
		nodeConfig.Node.InboxReader.Disable = true
		nodeConfig.Node.Sequencer.Enable = true // we sequence messages, but not to l1
		nodeConfig.Node.BatchPoster.Enable = false
	}

	if nodeConfig.Node.Sequencer.Enable {
		if nodeConfig.Node.ForwardingTarget != "" && nodeConfig.Node.ForwardingTarget != "null" {
			flag.Usage()
			panic("forwardingtarget set when sequencer enabled")
		}
	} else {
		if nodeConfig.Node.ForwardingTarget == "" {
			flag.Usage()
			panic("forwardingtarget unset, and not sequencer (can set to \"null\" to disable forwarding)")
		}
	}

	// Perform sanity check on mode
	_, err = nodeConfig.Node.DataAvailability.Mode()
	if err != nil {
		panic(err.Error())
	}

	if nodeConfig.Node.Wasm.RootPath != "" {
		validator.StaticNitroMachineConfig.RootPath = nodeConfig.Node.Wasm.RootPath
	} else {
		execfile, err := os.Executable()
		if err != nil {
			panic(err)
		}
		targetDir := filepath.Dir(filepath.Dir(execfile))
		validator.StaticNitroMachineConfig.RootPath = filepath.Join(targetDir, "machine")
	}

	wasmModuleRootString := nodeConfig.Node.Wasm.ModuleRoot
	if wasmModuleRootString == "" {
		fileToRead := path.Join(validator.StaticNitroMachineConfig.RootPath, "module_root")
		fileBytes, err := ioutil.ReadFile(fileToRead)
		if err != nil {
			if nodeConfig.Node.Validator.Enable && !nodeConfig.Node.Validator.WithoutBlockValidator {
				panic(fmt.Errorf("failed reading wasmModuleRoot from file, err %w", err))
			}
		}
		wasmModuleRootString = strings.TrimSpace(string(fileBytes))
		if len(wasmModuleRootString) > 64 {
			wasmModuleRootString = wasmModuleRootString[0:64]
		}
	}
	wasmModuleRoot := common.HexToHash(wasmModuleRootString)

	if nodeConfig.Node.Validator.Enable {
		if !nodeConfig.Node.EnableL1Reader {
			flag.Usage()
			panic("validator must read from L1")
		}
		if !nodeConfig.Node.BlockValidator.Enable && !nodeConfig.Node.Validator.WithoutBlockValidator {
			flag.Usage()
			panic("L1 validator requires block validator to safely function")
		}
	}

	if nodeConfig.Node.Validator.Enable {
		if !nodeConfig.Node.Validator.WithoutBlockValidator {
			if nodeConfig.Node.Wasm.CachePath != "" {
				validator.StaticNitroMachineConfig.InitialMachineCachePath = nodeConfig.Node.Wasm.CachePath
			}
			go func() {
				expectedRoot := wasmModuleRoot
				foundRoot, err := validator.GetInitialModuleRoot(ctx)
				if err != nil {
					panic(fmt.Errorf("failed reading wasmModuleRoot from machine: %w", err))
				}
				if foundRoot != expectedRoot {
					panic(fmt.Errorf("incompatible wasmModuleRoot expected: %v found %v", expectedRoot, foundRoot))
				} else {
					log.Info("loaded wasm machine", "wasmModuleRoot", foundRoot)
				}
			}()
		}
	}

	var l1client *ethclient.Client
	var deployInfo arbnode.RollupAddresses
	var l1TransactionOpts *bind.TransactOpts
	if nodeConfig.Node.EnableL1Reader {
		var err error

		l1client, err = ethclient.Dial(nodeConfig.L1.URL)
		if err != nil {
			flag.Usage()
			panic(err)
		}
		if nodeConfig.Node.BatchPoster.Enable || nodeConfig.Node.Validator.Enable {
			l1TransactionOpts, err = util.GetTransactOptsFromKeystore(
				l1wallet.Pathname,
				l1wallet.Account,
				*l1wallet.Password(),
				new(big.Int).SetUint64(nodeConfig.L1.ChainID),
			)
			if err != nil {
				panic(err)
			}
		}

		if nodeConfig.L1.Deployment == "" {
			flag.Usage()
			panic("no deployment specified")
		}
		rawDeployment, err := ioutil.ReadFile(nodeConfig.L1.Deployment)
		if err != nil {
			panic(err)
		}
		if err := json.Unmarshal(rawDeployment, &deployInfo); err != nil {
			panic(err)
		}
	}

	stackConf := node.DefaultConfig
	stackConf.DataDir = nodeConfig.Persistent.Chain
	stackConf.HTTPHost = nodeConfig.HTTP.Addr
	stackConf.HTTPPort = nodeConfig.HTTP.Port
	stackConf.HTTPVirtualHosts = nodeConfig.HTTP.VHosts
	stackConf.HTTPModules = nodeConfig.HTTP.API
	stackConf.WSHost = nodeConfig.WS.Addr
	stackConf.WSPort = nodeConfig.WS.Port
	stackConf.WSOrigins = nodeConfig.WS.Origins
	stackConf.WSModules = nodeConfig.WS.API
	stackConf.WSExposeAll = nodeConfig.WS.ExposeAll
	if nodeConfig.WS.ExposeAll {
		stackConf.WSModules = append(stackConf.WSModules, "personal")
	}
	stackConf.P2P.ListenAddr = ""
	stackConf.P2P.NoDial = true
	stackConf.P2P.NoDiscovery = true
	stack, err := node.New(&stackConf)
	if err != nil {
		flag.Usage()
		panic(err)
	}

	devPrivKeyStr := "e887f7d17d07cc7b8004053fb8826f6657084e88904bb61590e498ca04704cf2"
	devPrivKey, err := crypto.HexToECDSA(devPrivKeyStr)
	if err != nil {
		panic(err)
	}
	devAddr := crypto.PubkeyToAddress(devPrivKey.PublicKey)
	log.Info("Dev node funded private key", "priv", devPrivKeyStr)
	log.Info("Funded public address", "addr", devAddr)

	if l2wallet.Pathname != "" {
		mykeystore := keystore.NewPlaintextKeyStore(l2wallet.Pathname)
		stack.AccountManager().AddBackend(mykeystore)
		var account accounts.Account
		if mykeystore.HasAddress(devAddr) {
			account.Address = devAddr
			account, err = mykeystore.Find(account)
		} else {
			account, err = mykeystore.ImportECDSA(devPrivKey, *l2wallet.Password())
		}
		if err != nil {
			panic(err)
		}
		err = mykeystore.Unlock(account, *l2wallet.Password())
		if err != nil {
			panic(err)
		}
	}
	var initDataReader statetransfer.InitDataReader = nil

	chainDb, err := stack.OpenDatabaseWithFreezer("l2chaindata", 0, 0, "", "", false)
	if err != nil {
		utils.Fatalf("Failed to open database: %v", err)
	}

	if nodeConfig.ImportFile != "" {
		initDataReader, err = statetransfer.NewJsonInitDataReader(nodeConfig.ImportFile)
		if err != nil {
			panic(err)
		}
	} else if nodeConfig.DevInit {
		initData := statetransfer.ArbosInitializationInfo{
			Accounts: []statetransfer.AccountInitializationInfo{
				{
					Addr:       devAddr,
					EthBalance: new(big.Int).Mul(big.NewInt(params.Ether), big.NewInt(1000)),
					Nonce:      0,
				},
			},
		}
		initDataReader = statetransfer.NewMemoryInitDataReader(&initData)
		if err != nil {
			panic(err)
		}
	}

	chainConfig, err := arbos.GetChainConfig(new(big.Int).SetUint64(nodeConfig.L2.ChainID))
	if err != nil {
		panic(err)
	}

	var l2BlockChain *core.BlockChain
	if initDataReader != nil {
		blockReader, err := initDataReader.GetStoredBlockReader()
		if err != nil {
			panic(err)
		}
		blockNum, err := arbnode.ImportBlocksToChainDb(chainDb, blockReader)
		if err != nil {
			panic(err)
		}
		l2BlockChain, err = arbnode.WriteOrTestBlockChain(chainDb, arbnode.DefaultCacheConfigFor(stack), initDataReader, blockNum, chainConfig)
		if err != nil {
			panic(err)
		}

	} else {
		blocksInDb, err := chainDb.Ancients()
		if err != nil {
			panic(err)
		}
		if blocksInDb == 0 {
			panic("No initialization mode supplied, no blocks in Db")
		}
		l2BlockChain, err = arbnode.GetBlockChain(chainDb, arbnode.DefaultCacheConfigFor(stack), chainConfig)
		if err != nil {
			panic(err)
		}
	}

	// Check that this ArbOS state has the correct chain ID
	{
		statedb, err := l2BlockChain.State()
		if err != nil {
			panic(err)
		}
		arbosState, err := arbosState.OpenSystemArbosState(statedb, true)
		if err != nil {
			panic(err)
		}
		chainId, err := arbosState.ChainId()
		if err != nil {
			panic(err)
		}
		if chainId.Cmp(chainConfig.ChainID) != 0 {
			panic(fmt.Sprintf("attempted to launch node with chain ID %v on ArbOS state with chain ID %v", chainConfig.ChainID, chainId))
		}
	}

	_, err = arbnode.CreateNode(stack, chainDb, &nodeConf, l2BlockChain, l1client, &deployInfo, l1TransactionOpts, l1TransactionOpts, nil)
	if err != nil {
		panic(err)
	}
	if err := stack.Start(); err != nil {
		utils.Fatalf("Error starting protocol stack: %v\n", err)
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)

	<-sigint
	// cause future ctrl+c's to panic
	close(sigint)

	if err := stack.Close(); err != nil {
		utils.Fatalf("Error closing stack: %v\n", err)
	}
}

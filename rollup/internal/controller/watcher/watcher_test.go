package watcher

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"

	"scroll-tech/common/database"
	"scroll-tech/common/docker"
	"scroll-tech/common/types/encoding"

	"scroll-tech/database/migrate"

	"scroll-tech/rollup/internal/config"
)

var (
	// config
	cfg *config.Config

	base *docker.App

	// l2geth client
	l2Cli *ethclient.Client

	// block trace
	block1 *encoding.Block
	block2 *encoding.Block
)

func setupEnv(t *testing.T) (err error) {
	glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.LogfmtFormat()))
	glogger.Verbosity(log.LvlInfo)
	log.Root().SetHandler(glogger)

	// Load config.
	cfg, err = config.NewConfig("../../../conf/config.json")
	assert.NoError(t, err)

	base.RunImages(t)

	cfg.L2Config.RelayerConfig.SenderConfig.Endpoint = base.L1gethImg.Endpoint()
	cfg.L1Config.RelayerConfig.SenderConfig.Endpoint = base.L2gethImg.Endpoint()
	cfg.DBConfig = &database.Config{
		DSN:        base.DBConfig.DSN,
		DriverName: base.DBConfig.DriverName,
		MaxOpenNum: base.DBConfig.MaxOpenNum,
		MaxIdleNum: base.DBConfig.MaxIdleNum,
	}

	// Create l2geth client.
	l2Cli, err = base.L2Client()
	assert.NoError(t, err)

	templateBlockTrace1, err := os.ReadFile("../../../testdata/blockTrace_02.json")
	if err != nil {
		return err
	}
	// unmarshal blockTrace
	block1 = &encoding.Block{}
	if err = json.Unmarshal(templateBlockTrace1, block1); err != nil {
		return err
	}

	templateBlockTrace2, err := os.ReadFile("../../../testdata/blockTrace_03.json")
	if err != nil {
		return err
	}
	// unmarshal blockTrace
	block2 = &encoding.Block{}
	if err = json.Unmarshal(templateBlockTrace2, block2); err != nil {
		return err
	}
	return err
}

func setupDB(t *testing.T) *gorm.DB {
	db, err := database.InitDB(cfg.DBConfig)
	assert.NoError(t, err)
	sqlDB, err := db.DB()
	assert.NoError(t, err)
	assert.NoError(t, migrate.ResetDB(sqlDB))
	return db
}

func TestMain(m *testing.M) {
	base = docker.NewDockerApp()

	m.Run()

	base.Free()
}

func TestFunction(t *testing.T) {
	if err := setupEnv(t); err != nil {
		t.Fatal(err)
	}

	// Run l1 watcher test cases.
	t.Run("TestStartWatcher", testFetchContractEvent)
	t.Run("TestL1WatcherClientFetchBlockHeader", testL1WatcherClientFetchBlockHeader)
	t.Run("TestL1WatcherClientFetchContractEvent", testL1WatcherClientFetchContractEvent)
	t.Run("TestParseBridgeEventLogsL1QueueTransactionEventSignature", testParseBridgeEventLogsL1QueueTransactionEventSignature)
	t.Run("TestParseBridgeEventLogsL1CommitBatchEventSignature", testParseBridgeEventLogsL1CommitBatchEventSignature)
	t.Run("TestParseBridgeEventLogsL1FinalizeBatchEventSignature", testParseBridgeEventLogsL1FinalizeBatchEventSignature)

	// Run l2 watcher test cases.
	t.Run("TestFetchRunningMissingBlocks", testFetchRunningMissingBlocks)

	// Run chunk proposer test cases.
	t.Run("TestChunkProposerLimits", testChunkProposerLimits)

	// Run chunk proposer test cases.
	t.Run("TestBatchProposerLimits", testBatchProposerLimits)
	t.Run("TestBatchCommitGasAndCalldataSizeEstimation", testBatchCommitGasAndCalldataSizeEstimation)
}

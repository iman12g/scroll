package relayer

import (
	"context"
	"fmt"
	"math/big"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/scroll-tech/go-ethereum/accounts/abi"
	"github.com/scroll-tech/go-ethereum/crypto"
	"github.com/scroll-tech/go-ethereum/log"
	"gorm.io/gorm"

	"scroll-tech/common/types"

	bridgeAbi "scroll-tech/rollup/abi"
	"scroll-tech/rollup/internal/config"
	"scroll-tech/rollup/internal/controller/sender"
	"scroll-tech/rollup/internal/orm"
)

// Layer1Relayer is responsible for
//  1. fetch pending L1Message from db
//  2. relay pending message to layer 2 node
//
// Actions are triggered by new head from layer 1 geth node.
// @todo It's better to be triggered by watcher.
type Layer1Relayer struct {
	ctx context.Context

	cfg *config.RelayerConfig

	gasOracleSender *sender.Sender
	l1GasOracleABI  *abi.ABI

	lastGasPrice uint64
	minGasPrice  uint64
	gasPriceDiff uint64

	l1BlockOrm *orm.L1Block
	metrics    *l1RelayerMetrics
}

// NewLayer1Relayer will return a new instance of Layer1RelayerClient
func NewLayer1Relayer(ctx context.Context, db *gorm.DB, cfg *config.RelayerConfig, serviceType ServiceType, reg prometheus.Registerer) (*Layer1Relayer, error) {
	var gasOracleSender *sender.Sender
	var err error

	switch serviceType {
	case ServiceTypeL1GasOracle:
		gasOracleSender, err = sender.NewSender(ctx, cfg.SenderConfig, cfg.GasOracleSenderPrivateKey, "l1_relayer", "gas_oracle_sender", types.SenderTypeL1GasOracle, db, reg)
		if err != nil {
			addr := crypto.PubkeyToAddress(cfg.GasOracleSenderPrivateKey.PublicKey)
			return nil, fmt.Errorf("new gas oracle sender failed for address %s, err: %v", addr.Hex(), err)
		}

		// Ensure test features aren't enabled on the scroll mainnet.
		if gasOracleSender.GetChainID().Cmp(big.NewInt(534352)) == 0 && cfg.EnableTestEnvBypassFeatures {
			return nil, fmt.Errorf("cannot enable test env features in mainnet")
		}
	default:
		return nil, fmt.Errorf("invalid service type for l1_relayer: %v", serviceType)
	}

	var minGasPrice uint64
	var gasPriceDiff uint64
	if cfg.GasOracleConfig != nil {
		minGasPrice = cfg.GasOracleConfig.MinGasPrice
		gasPriceDiff = cfg.GasOracleConfig.GasPriceDiff
	} else {
		minGasPrice = 0
		gasPriceDiff = defaultGasPriceDiff
	}

	l1Relayer := &Layer1Relayer{
		cfg:        cfg,
		ctx:        ctx,
		l1BlockOrm: orm.NewL1Block(db),

		gasOracleSender: gasOracleSender,
		l1GasOracleABI:  bridgeAbi.L1GasPriceOracleABI,

		minGasPrice:  minGasPrice,
		gasPriceDiff: gasPriceDiff,
	}

	l1Relayer.metrics = initL1RelayerMetrics(reg)

	switch serviceType {
	case ServiceTypeL1GasOracle:
		go l1Relayer.handleL1GasOracleConfirmLoop(ctx)
	default:
		return nil, fmt.Errorf("invalid service type for l1_relayer: %v", serviceType)
	}

	return l1Relayer, nil
}

// ProcessGasPriceOracle imports gas price to layer2
func (r *Layer1Relayer) ProcessGasPriceOracle() {
	r.metrics.rollupL1RelayerGasPriceOraclerRunTotal.Inc()
	latestBlockHeight, err := r.l1BlockOrm.GetLatestL1BlockHeight(r.ctx)
	if err != nil {
		log.Warn("Failed to fetch latest L1 block height from db", "err", err)
		return
	}

	blocks, err := r.l1BlockOrm.GetL1Blocks(r.ctx, map[string]interface{}{
		"number": latestBlockHeight,
	})
	if err != nil {
		log.Error("Failed to GetL1Blocks from db", "height", latestBlockHeight, "err", err)
		return
	}
	if len(blocks) != 1 {
		log.Error("Block not exist", "height", latestBlockHeight)
		return
	}
	block := blocks[0]

	if types.GasOracleStatus(block.GasOracleStatus) == types.GasOraclePending {
		expectedDelta := r.lastGasPrice * r.gasPriceDiff / gasPriceDiffPrecision
		if r.lastGasPrice > 0 && expectedDelta == 0 {
			expectedDelta = 1
		}
		// last is undefine or (block.BaseFee >= minGasPrice && exceed diff)
		if r.lastGasPrice == 0 || (block.BaseFee >= r.minGasPrice && (block.BaseFee >= r.lastGasPrice+expectedDelta || block.BaseFee <= r.lastGasPrice-expectedDelta)) {
			baseFee := big.NewInt(int64(block.BaseFee))
			data, err := r.l1GasOracleABI.Pack("setL1BaseFee", baseFee)
			if err != nil {
				log.Error("Failed to pack setL1BaseFee", "block.Hash", block.Hash, "block.Height", block.Number, "block.BaseFee", block.BaseFee, "err", err)
				return
			}

			hash, err := r.gasOracleSender.SendTransaction(block.Hash, &r.cfg.GasPriceOracleContractAddress, big.NewInt(0), data, 0)
			if err != nil {
				log.Error("Failed to send setL1BaseFee tx to layer2 ", "block.Hash", block.Hash, "block.Height", block.Number, "err", err)
				return
			}

			err = r.l1BlockOrm.UpdateL1GasOracleStatusAndOracleTxHash(r.ctx, block.Hash, types.GasOracleImporting, hash.String())
			if err != nil {
				log.Error("UpdateGasOracleStatusAndOracleTxHash failed", "block.Hash", block.Hash, "block.Height", block.Number, "err", err)
				return
			}
			r.lastGasPrice = block.BaseFee
			r.metrics.rollupL1RelayerLastGasPrice.Set(float64(r.lastGasPrice))
			log.Info("Update l1 base fee", "txHash", hash.String(), "baseFee", baseFee)
		}
	}
}

func (r *Layer1Relayer) handleConfirmation(cfm *sender.Confirmation) {
	switch cfm.SenderType {
	case types.SenderTypeL1GasOracle:
		var status types.GasOracleStatus
		if cfm.IsSuccessful {
			status = types.GasOracleImported
			r.metrics.rollupL1UpdateGasOracleConfirmedTotal.Inc()
			log.Info("UpdateGasOracleTxType transaction confirmed in layer2", "confirmation", cfm)
		} else {
			status = types.GasOracleImportedFailed
			r.metrics.rollupL1UpdateGasOracleConfirmedFailedTotal.Inc()
			log.Warn("UpdateGasOracleTxType transaction confirmed but failed in layer2", "confirmation", cfm)
		}

		err := r.l1BlockOrm.UpdateL1GasOracleStatusAndOracleTxHash(r.ctx, cfm.ContextID, status, cfm.TxHash.String())
		if err != nil {
			log.Warn("UpdateL1GasOracleStatusAndOracleTxHash failed", "confirmation", cfm, "err", err)
		}
	default:
		log.Warn("Unknown transaction type", "confirmation", cfm)
	}

	log.Info("Transaction confirmed in layer2", "confirmation", cfm)
}

func (r *Layer1Relayer) handleL1GasOracleConfirmLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cfm := <-r.gasOracleSender.ConfirmChan():
			r.handleConfirmation(cfm)
		}
	}
}

package cosmosHub

import (
	"math/big"
	"strings"

	"github.com/anyswap/CrossChain-Router/v3/common"
	"github.com/anyswap/CrossChain-Router/v3/log"
	"github.com/anyswap/CrossChain-Router/v3/router"
	"github.com/anyswap/CrossChain-Router/v3/tokens"
	"github.com/anyswap/CrossChain-Router/v3/tokens/cosmos"
)

const (
	CoinSymbol   = "uatom"
	TransferType = "transfer"
)

// VerifyMsgHash verify msg hash
func (b *Bridge) VerifyMsgHash(rawTx interface{}, msgHashes []string) (err error) {
	return tokens.ErrNotImplemented
}

// VerifyTransaction impl
func (b *Bridge) VerifyTransaction(txHash string, args *tokens.VerifyArgs) (*tokens.SwapTxInfo, error) {
	swapType := args.SwapType
	logIndex := args.LogIndex
	allowUnstable := args.AllowUnstable

	switch swapType {
	case tokens.ERC20SwapType:
		return b.verifySwapoutTx(txHash, logIndex, allowUnstable)
	default:
		return nil, tokens.ErrSwapTypeNotSupported
	}
}

func (b *Bridge) verifySwapoutTx(txHash string, logIndex int, allowUnstable bool) (*tokens.SwapTxInfo, error) {
	swapInfo := &tokens.SwapTxInfo{SwapInfo: tokens.SwapInfo{ERC20SwapInfo: &tokens.ERC20SwapInfo{}}}
	swapInfo.SwapType = tokens.ERC20SwapType          // SwapType
	swapInfo.Hash = txHash                            // Hash
	swapInfo.LogIndex = logIndex                      // LogIndex
	swapInfo.FromChainID = b.ChainConfig.GetChainID() // FromChainID

	txr, err := b.GetTransactionByHash(txHash)
	if err != nil {
		log.Debug("[verifySwapin] "+b.ChainConfig.BlockChain+" Bridge::GetTransaction fail", "tx", txHash, "err", err)
		return swapInfo, tokens.ErrTxNotFound
	}

	if txHeight, err := b.checkTxStatus(txr, allowUnstable); err != nil {
		return swapInfo, err
	} else {
		swapInfo.Height = txHeight // Height
	}

	tx := txr.TxResponse.Tx.(*TxBody)
	if err := ParseMemo(swapInfo, tx.Memo); err != nil {
		return swapInfo, err
	}

	if err := b.ParseAmountTotal(txr, swapInfo); err != nil {
		return swapInfo, err
	}

	if checkErr := b.checkSwapoutInfo(swapInfo); checkErr != nil {
		return swapInfo, checkErr
	}

	if !allowUnstable {
		log.Info("verify swapout pass",
			"token", swapInfo.ERC20SwapInfo.Token, "from", swapInfo.From, "to", swapInfo.To,
			"bind", swapInfo.Bind, "value", swapInfo.Value, "txid", swapInfo.Hash,
			"height", swapInfo.Height, "timestamp", swapInfo.Timestamp, "logIndex", swapInfo.LogIndex)
	}

	return swapInfo, nil
}

func (b *Bridge) checkSwapoutInfo(swapInfo *tokens.SwapTxInfo) error {
	if strings.EqualFold(swapInfo.From, swapInfo.To) {
		return tokens.ErrTxWithWrongSender
	}

	erc20SwapInfo := swapInfo.ERC20SwapInfo

	fromTokenCfg := b.GetTokenConfig(erc20SwapInfo.Token)
	if fromTokenCfg == nil || erc20SwapInfo.TokenID == "" {
		return tokens.ErrMissTokenConfig
	}

	multichainToken := router.GetCachedMultichainToken(erc20SwapInfo.TokenID, swapInfo.ToChainID.String())
	if multichainToken == "" {
		log.Warn("get multichain token failed", "tokenID", erc20SwapInfo.TokenID, "chainID", swapInfo.ToChainID, "txid", swapInfo.Hash)
		return tokens.ErrMissTokenConfig
	}

	toBridge := router.GetBridgeByChainID(swapInfo.ToChainID.String())
	if toBridge == nil {
		return tokens.ErrNoBridgeForChainID
	}

	toTokenCfg := toBridge.GetTokenConfig(multichainToken)
	if toTokenCfg == nil {
		log.Warn("get token config failed", "chainID", swapInfo.ToChainID, "token", multichainToken)
		return tokens.ErrMissTokenConfig
	}

	if !tokens.CheckTokenSwapValue(swapInfo, fromTokenCfg.Decimals, toTokenCfg.Decimals) {
		return tokens.ErrTxWithWrongValue
	}

	bindAddr := swapInfo.Bind
	if !toBridge.IsValidAddress(bindAddr) {
		log.Warn("wrong bind address in swapin", "bind", bindAddr)
		return tokens.ErrWrongBindAddress
	}
	return nil
}

func (b *Bridge) checkTxStatus(txres *cosmos.GetTxResponse, allowUnstable bool) (txHeight uint64, err error) {
	txHeight = uint64(txres.TxResponse.Height)

	if txres.TxResponse.Code != 0 {
		return txHeight, tokens.ErrTxWithWrongStatus
	}

	if !allowUnstable {
		if h, err := b.GetLatestBlockNumber(); err != nil {
			return txHeight, err
		} else {
			if h < txHeight+b.GetChainConfig().Confirmations {
				return txHeight, tokens.ErrTxNotStable
			}
			if h < b.ChainConfig.InitialHeight {
				return txHeight, tokens.ErrTxBeforeInitialHeight
			}
		}
	}
	return txHeight, err
}

func ParseMemo(swapInfo *tokens.SwapTxInfo, memo string) error {
	fields := strings.Split(memo, ":")
	if len(fields) == 2 {
		if toChainID, err := common.GetBigIntFromStr(fields[1]); err != nil {
			return err
		} else {
			dstBridge := router.GetBridgeByChainID(fields[1])
			if dstBridge != nil && dstBridge.IsValidAddress(fields[0]) {
				swapInfo.Bind = fields[0]      // Bind
				swapInfo.ToChainID = toChainID // ToChainID
				swapInfo.To = fields[0]        // To
				return nil
			}
		}
	}
	return tokens.ErrTxWithWrongMemo
}

//nolint:goconst // allow big check logic
func (b *Bridge) ParseAmountTotal(txres *cosmos.GetTxResponse, swapInfo *tokens.SwapTxInfo) error {
	mpc := b.GetRouterContract("")
	amount := big.NewInt(0)
	for _, log := range txres.TxResponse.Logs {
		for _, event := range log.Events {
			if event.Type == TransferType && len(event.Attributes)%3 == 0 {
				for i := 0; i < len(event.Attributes); i += 3 {
					// attribute key mismatch
					if event.Attributes[i].Key == "recipient" &&
						event.Attributes[i+1].Key == "sender" &&
						event.Attributes[i+2].Key == "amount" {
						// receiver mismatch
						if common.IsEqualIgnoreCase(event.Attributes[i].Value, mpc) {
							if recvCoins, err := cosmos.ParseCoinsNormalized(event.Attributes[i+2].Value); err == nil {
								recvAmount := recvCoins.AmountOfNoDenomValidation(CoinSymbol)
								if !recvAmount.IsNil() && !recvAmount.IsZero() {
									if swapInfo.From == "" {
										swapInfo.From = event.Attributes[i+1].Value
									}
									amount.Add(amount, recvAmount.BigInt())
								}
							}
						}
					}
				}
			}
		}
	}
	if amount.Cmp(big.NewInt(0)) > 0 {
		swapInfo.Value = amount
		return nil
	}
	return tokens.ErrTxWithWrongValue
}

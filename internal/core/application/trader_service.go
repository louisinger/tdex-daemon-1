package application

import (
	"context"
	"encoding/hex"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tdex-network/tdex-daemon/config"
	"github.com/tdex-network/tdex-daemon/internal/core/domain"
	"github.com/tdex-network/tdex-daemon/pkg/bufferutil"
	"github.com/tdex-network/tdex-daemon/pkg/explorer"
	mm "github.com/tdex-network/tdex-daemon/pkg/marketmaking"
	pkgswap "github.com/tdex-network/tdex-daemon/pkg/swap"
	"github.com/tdex-network/tdex-daemon/pkg/transactionutil"
	"github.com/tdex-network/tdex-daemon/pkg/wallet"
	pb "github.com/tdex-network/tdex-protobuf/generated/go/swap"
	"github.com/vulpemventures/go-elements/address"
	"github.com/vulpemventures/go-elements/pset"
)

type TraderService interface {
	GetTradableMarkets(ctx context.Context) ([]MarketWithFee, error)
	GetMarketPrice(
		ctx context.Context,
		market Market,
		tradeType int,
		amount uint64,
	) (*PriceWithFee, error)
	TradePropose(
		ctx context.Context,
		market Market,
		tradeType int,
		swapRequest *pb.SwapRequest,
	) (*pb.SwapAccept, *pb.SwapFail, uint64, error)
	TradeComplete(
		ctx context.Context,
		swapComplete *pb.SwapComplete,
		swapFail *pb.SwapFail,
	) (string, *pb.SwapFail, error)
}

type traderService struct {
	marketRepository  domain.MarketRepository
	tradeRepository   domain.TradeRepository
	vaultRepository   domain.VaultRepository
	unspentRepository domain.UnspentRepository
	explorerSvc       explorer.Service
}

func NewTraderService(
	marketRepository domain.MarketRepository,
	tradeRepository domain.TradeRepository,
	vaultRepository domain.VaultRepository,
	unspentRepository domain.UnspentRepository,
	explorerSvc explorer.Service,
) TraderService {
	return &traderService{
		marketRepository:  marketRepository,
		tradeRepository:   tradeRepository,
		vaultRepository:   vaultRepository,
		unspentRepository: unspentRepository,
		explorerSvc:       explorerSvc,
	}
}

// Markets is the domain controller for the Markets RPC
func (t *traderService) GetTradableMarkets(ctx context.Context) (
	[]MarketWithFee,
	error,
) {
	tradableMarkets, err := t.marketRepository.GetTradableMarkets(ctx)
	if err != nil {
		return nil, err
	}

	marketsWithFee := make([]MarketWithFee, 0, len(tradableMarkets))
	for _, mkt := range tradableMarkets {
		marketsWithFee = append(marketsWithFee, MarketWithFee{
			Market: Market{
				BaseAsset:  mkt.BaseAssetHash(),
				QuoteAsset: mkt.QuoteAssetHash(),
			},
			Fee: Fee{
				FeeAsset:   mkt.FeeAsset(),
				BasisPoint: mkt.Fee(),
			},
		})
	}

	return marketsWithFee, nil
}

// MarketPrice is the domain controller for the MarketPrice RPC.
func (t *traderService) GetMarketPrice(
	ctx context.Context,
	market Market,
	tradeType int,
	amount uint64,
) (*PriceWithFee, error) {

	// Checks if base asset is correct
	if market.BaseAsset != config.GetString(config.BaseAssetKey) {
		return nil, domain.ErrMarketNotExist
	}
	//Checks if market exist
	mkt, mktAccountIndex, err := t.marketRepository.GetMarketByAsset(
		ctx,
		market.QuoteAsset,
	)
	if err != nil {
		return nil, err
	}

	if !mkt.IsTradable() {
		return nil, domain.ErrMarketIsClosed
	}

	unspents, _, _, _, err := t.getUnspentsBlindingsAndDerivationPathsForAccount(ctx, mktAccountIndex)
	if err != nil {
		return nil, err
	}

	price, previewAmount, err := getPriceAndPreviewForMarket(unspents, mkt, tradeType, amount)
	if err != nil {
		return nil, err
	}

	return &PriceWithFee{
		Price: price,
		Fee: Fee{
			FeeAsset:   mkt.FeeAsset(),
			BasisPoint: mkt.Fee(),
		},
		Amount: previewAmount,
	}, nil
}

// TradePropose is the domain controller for the TradePropose RPC
func (t *traderService) TradePropose(
	ctx context.Context,
	market Market,
	tradeType int,
	swapRequest *pb.SwapRequest,
) (
	swapAccept *pb.SwapAccept,
	swapFail *pb.SwapFail,
	swapExpiryTime uint64,
	err error,
) {
	mkt, marketAccountIndex, _err := t.marketRepository.GetMarketByAsset(
		ctx,
		market.QuoteAsset,
	)
	if _err != nil {
		err = _err
		return
	}

	// get all unspents for market account (both as []domain.Unspents and as
	// []explorer.Utxo)along with private blinding keys and signing derivation
	// paths for respectively unblinding and signing them later
	marketUnspents, marketUtxos, marketBlindingKeysByScript, marketDerivationPaths, _err :=
		t.getUnspentsBlindingsAndDerivationPathsForAccount(ctx, marketAccountIndex)
	if _err != nil {
		err = _err
		return
	}

	// ... and the same for fee account (we'll need to top-up fees)
	_, feeUtxos, feeBlindingKeysByScript, feeDerivationPaths, _err :=
		t.getUnspentsBlindingsAndDerivationPathsForAccount(ctx, domain.FeeAccount)
	if _err != nil {
		err = _err
		return
	}

	amount := swapRequest.AmountR
	if tradeType == TradeSell {
		amount = swapRequest.AmountP
	}

	_, previewAmount, _err := getPriceAndPreviewForMarket(
		marketUnspents,
		mkt, tradeType, amount,
	)
	if _err != nil {
		err = _err
		return
	}

	var mnemonic []string
	var tradeID uuid.UUID
	var selectedUnspents []explorer.Utxo
	var outputBlindingKeyByScript map[string][]byte
	var outputDerivationPath, changeDerivationPath, feeChangeDerivationPath string

	// derive output and change address for market, and change address for fee account
	if err := t.vaultRepository.UpdateVault(
		ctx,
		nil,
		"",
		func(v *domain.Vault) (*domain.Vault, error) {
			mnemonic, err = v.Mnemonic()
			if err != nil {
				return nil, err
			}
			outputAddress, outputScript, _, err := v.DeriveNextExternalAddressForAccount(marketAccountIndex)
			if err != nil {
				return nil, err
			}
			_, changeScript, _, err := v.DeriveNextInternalAddressForAccount(marketAccountIndex)
			if err != nil {
				return nil, err
			}
			_, feeChangeScript, _,
				err := v.DeriveNextInternalAddressForAccount(domain.FeeAccount)
			if err != nil {
				return nil, err
			}
			marketAccount, _ := v.AccountByIndex(marketAccountIndex)
			feeAccount, _ := v.AccountByIndex(domain.FeeAccount)

			outputBlindingKeyByScript = blindingKeyByScriptFromCTAddress(outputAddress)
			outputDerivationPath, _ = marketAccount.DerivationPathByScript(outputScript)
			changeDerivationPath, _ = marketAccount.DerivationPathByScript(changeScript)
			feeChangeDerivationPath, _ = feeAccount.DerivationPathByScript(feeChangeScript)

			return v, nil
		}); err != nil {
		return nil, nil, 0, err
	}

	// parse swap proposal and possibly accept
	if err := t.tradeRepository.UpdateTrade(
		ctx,
		nil,
		func(trade *domain.Trade) (*domain.Trade, error) {
			ok, err := trade.Propose(swapRequest, market.QuoteAsset, nil)
			if err != nil {
				return nil, err
			}
			if !ok {
				swapFail = trade.SwapFailMessage()
				return trade, nil
			}

			if !isValidTradePrice(swapRequest, tradeType, previewAmount) {
				trade.Fail(
					swapRequest.GetId(),
					domain.ProposalRejectedStatus,
					pkgswap.ErrCodeInvalidSwapRequest,
					"bad pricing",
				)
				return trade, nil
			}
			tradeID = trade.ID()

			acceptSwapResult, err := acceptSwap(acceptSwapOpts{
				mnemonic:                   mnemonic,
				swapRequest:                swapRequest,
				marketUnspents:             marketUtxos,
				feeUnspents:                feeUtxos,
				marketBlindingKeysByScript: marketBlindingKeysByScript,
				feeBlindingKeysByScript:    feeBlindingKeysByScript,
				outputBlindingKeyByScript:  outputBlindingKeyByScript,
				marketDerivationPaths:      marketDerivationPaths,
				feeDerivationPaths:         feeDerivationPaths,
				outputDerivationPath:       outputDerivationPath,
				changeDerivationPath:       changeDerivationPath,
				feeChangeDerivationPath:    feeChangeDerivationPath,
			})
			if err != nil {
				return nil, err
			}

			ok, err = trade.Accept(
				acceptSwapResult.psetBase64,
				acceptSwapResult.inputBlindingKeys,
				acceptSwapResult.outputBlindingKeys,
			)
			if err != nil {
				return nil, err
			}
			if !ok {
				swapFail = trade.SwapFailMessage()
			} else {
				swapAccept = trade.SwapAcceptMessage()
				swapExpiryTime = trade.SwapExpiryTime()
				selectedUnspents = acceptSwapResult.selectedUnspents
			}

			return trade, nil
		}); err != nil {
		return nil, nil, 0, err
	}

	selectedUnspentKeys := getUnspentKeys(selectedUnspents)
	if err := t.unspentRepository.LockUnspents(
		ctx,
		selectedUnspentKeys,
		tradeID,
	); err != nil {
		return nil, nil, 0, err
	}

	return
}

// TradeComplete is the domain controller for the TradeComplete RPC
func (t *traderService) TradeComplete(
	ctx context.Context,
	swapComplete *pb.SwapComplete,
	swapFail *pb.SwapFail,
) (string, *pb.SwapFail, error) {
	if swapFail != nil {
		swapFailMsg, err := t.tradeFail(ctx, swapFail)
		return "", swapFailMsg, err
	}

	return t.tradeComplete(ctx, swapComplete)
}

func (t *traderService) tradeComplete(ctx context.Context, swapComplete *pb.SwapComplete) (txID string, swapFail *pb.SwapFail, err error) {
	trade, err := t.tradeRepository.GetTradeBySwapAcceptID(ctx, swapComplete.GetAcceptId())
	if err != nil {
		return "", nil, err
	}

	tradeID := trade.ID()
	err = t.tradeRepository.UpdateTrade(
		ctx,
		&tradeID,
		func(trade *domain.Trade) (*domain.Trade, error) {
			psetBase64 := swapComplete.GetTransaction()
			opts := wallet.FinalizeAndExtractTransactionOpts{
				PsetBase64: psetBase64,
			}
			txHex, txHash, err := wallet.FinalizeAndExtractTransaction(opts)
			if err != nil {
				return nil, err
			}

			ok, err := trade.Complete(psetBase64, txID)
			if err != nil {
				return nil, err
			}
			if !ok {
				swapFail = trade.SwapFailMessage()
				return trade, nil
			}

			if _, err := t.explorerSvc.BroadcastTransaction(txHex); err != nil {
				return nil, err
			}

			txID = txHash
			return trade, nil
		},
	)
	return
}

func (t *traderService) tradeFail(ctx context.Context, swapFail *pb.SwapFail) (*pb.SwapFail, error) {
	swapID := swapFail.GetMessageId()
	trade, err := t.tradeRepository.GetTradeBySwapAcceptID(ctx, swapID)
	if err != nil {
		return nil, err
	}

	tradeID := trade.ID()
	err = t.tradeRepository.UpdateTrade(
		ctx,
		&tradeID,
		func(trade *domain.Trade) (*domain.Trade, error) {
			trade.Fail(
				swapID,
				domain.FailedToCompleteStatus,
				pkgswap.ErrCodeFailedToComplete,
				"set failed by counter-party",
			)
			return trade, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return swapFail, nil
}

func (t *traderService) getUnspentsBlindingsAndDerivationPathsForAccount(
	ctx context.Context,
	account int,
) (
	[]domain.Unspent,
	[]explorer.Utxo,
	map[string][]byte,
	map[string]string,
	error,
) {
	derivedAddresses, blindingKeys, err := t.vaultRepository.
		GetAllDerivedAddressesAndBlindingKeysForAccount(ctx, account)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	unspents, err := t.unspentRepository.GetAvailableUnspentsForAddresses(
		ctx,
		derivedAddresses,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	utxos := make([]explorer.Utxo, 0, len(unspents))
	for _, unspent := range unspents {
		utxos = append(utxos, unspent.ToUtxo())
	}

	scripts := make([]string, 0, len(derivedAddresses))
	for _, addr := range derivedAddresses {
		script, _ := address.ToOutputScript(addr, *config.GetNetwork())
		scripts = append(scripts, hex.EncodeToString(script))
	}
	derivationPaths, _ := t.vaultRepository.GetDerivationPathByScript(
		ctx,
		account,
		scripts,
	)

	blindingKeysByScript := map[string][]byte{}
	for i, addr := range derivedAddresses {
		script, _ := address.ToOutputScript(addr, *config.GetNetwork())
		blindingKeysByScript[hex.EncodeToString(script)] = blindingKeys[i]
	}

	return unspents, utxos, blindingKeysByScript, derivationPaths, nil
}

func blindingKeyByScriptFromCTAddress(addr string) map[string][]byte {
	script, _ := address.ToOutputScript(addr, *config.GetNetwork())
	blech32, _ := address.FromBlech32(addr)
	return map[string][]byte{
		hex.EncodeToString(script): blech32.PublicKey,
	}
}

type acceptSwapOpts struct {
	mnemonic                   []string
	swapRequest                *pb.SwapRequest
	marketUnspents             []explorer.Utxo
	feeUnspents                []explorer.Utxo
	marketBlindingKeysByScript map[string][]byte
	feeBlindingKeysByScript    map[string][]byte
	outputBlindingKeyByScript  map[string][]byte
	marketDerivationPaths      map[string]string
	feeDerivationPaths         map[string]string
	outputDerivationPath       string
	changeDerivationPath       string
	feeChangeDerivationPath    string
}

type acceptSwapResult struct {
	psetBase64         string
	selectedUnspents   []explorer.Utxo
	inputBlindingKeys  map[string][]byte
	outputBlindingKeys map[string][]byte
}

func acceptSwap(opts acceptSwapOpts) (res acceptSwapResult, err error) {
	w, err := wallet.NewWalletFromMnemonic(wallet.NewWalletFromMnemonicOpts{
		SigningMnemonic: opts.mnemonic,
	})
	if err != nil {
		return
	}
	network := config.GetNetwork()

	// fill swap request transaction with daemon's inputs and outputs
	psetBase64, selectedUnspentsForSwap, err := w.UpdateSwapTx(wallet.UpdateSwapTxOpts{
		PsetBase64:           opts.swapRequest.GetTransaction(),
		Unspents:             opts.marketUnspents,
		InputAmount:          opts.swapRequest.GetAmountP(),
		InputAsset:           opts.swapRequest.GetAssetP(),
		OutputAmount:         opts.swapRequest.GetAmountR(),
		OutputAsset:          opts.swapRequest.GetAssetR(),
		OutputDerivationPath: opts.outputDerivationPath,
		ChangeDerivationPath: opts.changeDerivationPath,
		Network:              network,
	})
	if err != nil {
		return
	}

	// top-up fees using fee account. Note that the fee output is added after
	// blinding the transaction because it's explicit and must not be blinded
	psetWithFeesResult, err := w.UpdateTx(wallet.UpdateTxOpts{
		PsetBase64:        psetBase64,
		Unspents:          opts.feeUnspents,
		MilliSatsPerBytes: domain.MinMilliSatPerByte,
		Network:           network,
		ChangePathsByAsset: map[string]string{
			network.AssetID: opts.feeChangeDerivationPath,
		},
	})
	if err != nil {
		return
	}

	// concat the selected unspents for paying fees with those for completing the
	// swap in order to get the full list of selected inputs
	selectedUnspents := append(selectedUnspentsForSwap, psetWithFeesResult.SelectedUnspents...)

	// get blinding private keys for selected inputs
	unspentsBlindingKeys := mergeBlindingKeys(opts.marketBlindingKeysByScript, opts.feeBlindingKeysByScript)
	selectedInBlindingKeys := getSelectedBlindingKeys(unspentsBlindingKeys, selectedUnspents)
	// ... and merge with those contained into the swapRequest (trader keys)
	inputBlindingKeys := mergeBlindingKeys(opts.swapRequest.GetInputBlindingKey(), selectedInBlindingKeys)

	// same for output  public blinding keys
	outputBlindingKeys := mergeBlindingKeys(
		opts.outputBlindingKeyByScript,
		psetWithFeesResult.ChangeOutputsBlindingKeys,
		opts.swapRequest.GetOutputBlindingKey(),
	)

	// blind the transaction
	blindedPset, err := w.BlindSwapTransaction(wallet.BlindSwapTransactionOpts{
		PsetBase64:         psetWithFeesResult.PsetBase64,
		InputBlindingKeys:  inputBlindingKeys,
		OutputBlindingKeys: outputBlindingKeys,
	})

	// add the explicit fee output to the tx
	blindedPlusFees, err := w.UpdateTx(wallet.UpdateTxOpts{
		PsetBase64: blindedPset,
		Outputs:    transactionutil.NewFeeOutput(psetWithFeesResult.FeeAmount),
		Network:    network,
	})
	if err != nil {
		return
	}

	// get the indexes of the inputs of the tx to sign
	inputsToSign := getInputsIndexes(psetWithFeesResult.PsetBase64, selectedUnspents)
	// get the derivation paths of the selected inputs
	unspentsDerivationPaths := mergeDerivationPaths(opts.marketDerivationPaths, opts.feeDerivationPaths)
	derivationPaths := getSelectedDerivationPaths(unspentsDerivationPaths, selectedUnspents)

	signedPsetBase64 := blindedPlusFees.PsetBase64
	for i, inIndex := range inputsToSign {
		signedPsetBase64, err = w.SignInput(wallet.SignInputOpts{
			PsetBase64:     signedPsetBase64,
			InIndex:        inIndex,
			DerivationPath: derivationPaths[i],
		})
	}

	res = acceptSwapResult{
		psetBase64:         signedPsetBase64,
		selectedUnspents:   selectedUnspents,
		inputBlindingKeys:  inputBlindingKeys,
		outputBlindingKeys: outputBlindingKeys,
	}

	return
}

func getInputsIndexes(psetBase64 string, unspents []explorer.Utxo) []uint32 {
	indexes := make([]uint32, 0, len(unspents))

	ptx, _ := pset.NewPsetFromBase64(psetBase64)
	for _, u := range unspents {
		for i, in := range ptx.UnsignedTx.Inputs {
			if u.Hash() == bufferutil.TxIDFromBytes(in.Hash) && u.Index() == in.Index {
				indexes = append(indexes, uint32(i))
				break
			}
		}
	}
	return indexes
}

func getUnspentKeys(unspents []explorer.Utxo) []domain.UnspentKey {
	keys := make([]domain.UnspentKey, 0, len(unspents))
	for _, u := range unspents {
		keys = append(keys, domain.UnspentKey{
			TxID: u.Hash(),
			VOut: u.Index(),
		})
	}
	return keys
}

func mergeBlindingKeys(maps ...map[string][]byte) map[string][]byte {
	merge := make(map[string][]byte, 0)
	for _, m := range maps {
		for k, v := range m {
			merge[k] = v
		}
	}
	return merge
}

func mergeDerivationPaths(maps ...map[string]string) map[string]string {
	merge := make(map[string]string, 0)
	for _, m := range maps {
		for k, v := range m {
			merge[k] = v
		}
	}
	return merge
}

func getSelectedDerivationPaths(derivationPaths map[string]string, unspents []explorer.Utxo) []string {
	selectedPaths := make([]string, 0)
	for _, unspent := range unspents {
		script := hex.EncodeToString(unspent.Script())
		selectedPaths = append(selectedPaths, derivationPaths[script])
	}
	return selectedPaths
}

func getSelectedBlindingKeys(blindingKeys map[string][]byte, unspents []explorer.Utxo) map[string][]byte {
	selectedKeys := map[string][]byte{}
	for _, unspent := range unspents {
		script := hex.EncodeToString(unspent.Script())
		selectedKeys[script] = blindingKeys[script]
	}
	return selectedKeys
}

// getPriceAndPreviewForMarket returns the current price of a market, along
// with a amount preview for a BUY or SELL trade.
// Depending on the strategy set for the market, an input amount might be
// required to calculate the preview amount.
// In the specific, if the market strategy is not pluggable, the preview amount
// is calculated with either InGivenOut or OutGivenIn methods of the
// MakingFormula interface. Otherwise, the price is simply retrieved from the
// domain.Market instance and the preview amount is calculated by applying the
// market fees within the conversion.
// The incoming amount always represents an amount of the market's base asset,
// therefore a preview amount for the correspoing quote asset is returned.
func getPriceAndPreviewForMarket(
	unspents []domain.Unspent,
	market *domain.Market,
	tradeType int,
	amount uint64,
) (
	price Price,
	previewAmount uint64,
	err error,
) {
	if market.IsStrategyPluggable() {
		previewAmount = calcPreviewAmount(market, tradeType, amount)
		price = Price{
			BasePrice:  market.BaseAssetPrice(),
			QuotePrice: market.QuoteAssetPrice(),
		}
		return
	}

	return previewFromFormula(unspents, market, tradeType, amount)
}

func getBalanceByAsset(unspents []domain.Unspent) map[string]uint64 {
	balances := map[string]uint64{}
	for _, unspent := range unspents {
		if _, ok := balances[unspent.AssetHash]; !ok {
			balances[unspent.AssetHash] = 0
		}
		balances[unspent.AssetHash] += unspent.Value
	}
	return balances
}

// calcPreviewAmount calculates the amount of a market's quote asset due,
// depending on the trade type and the base asset amount provided.
// The market fees are either added or subtracted to the converted amount
// basing on the trade type.
func calcPreviewAmount(market *domain.Market, tradeType int, amount uint64) uint64 {
	if tradeType == TradeBuy {
		return calcProposeAmount(
			amount,
			market.Fee(),
			market.QuoteAssetPrice(),
		)
	}

	return calcExpectedAmount(
		amount,
		market.Fee(),
		market.QuoteAssetPrice(),
	)
}

// calcProposeAmount returns the quote asset amount due for a BUY trade, that,
// remind, expresses a willing of buying a certain amount of the market's base
// asset.
// The market fees can be collected in either base or quote asset, but this is
// not relevant when calculating the preview amount. The reason is explained
// with the following example:
//
// Alice wants to BUY 0.1 LBTC in exchange for USDT (hence LBTC/USDT market).
// Lets assume the provider holds 10 LBTC and 65000 USDT in his reserves, so
// the USDT/LBTC price is 6500.
// Depending on how the fees are collected we have:
// - fee_asset = LBTC
//		feeAmount = lbtcAmount * feePercentage
// 		usdtAmount = (lbtcAmount + feeAmount) * price =
//			= (lbtcAmount + lbtcAmount * feeAmount) * price =
//			= (1 + feeAmount) * lbtcAmount * price = 1.25 * 0.1 * 6500 = 812,5 USDT
// - fee_asset = USDT
//		cAmount = lbtcAmount * price
// 		feeAmount = cAmount * feePercentage
// 		usdtAmount = cAmount + feeAmount =
//			= (lbtcAmount * price) + (lbtcAmount * price * feePercentage)
// 			= lbtcAmount * price * (1 + feePercentage) = 0.1  * 6500 * 1,25 = 812,5 USDT
func calcProposeAmount(
	amount uint64,
	feeAmount int64,
	price decimal.Decimal,
) uint64 {
	feePercentage := decimal.NewFromInt(feeAmount).Div(decimal.NewFromInt(100))
	amountR := decimal.NewFromInt(int64(amount))

	// amountP = amountR * price * (1 + feePercentage)
	amountP := amountR.Mul(price).Mul(decimal.NewFromInt(1).Add(feePercentage))
	return amountP.BigInt().Uint64()
}

func calcExpectedAmount(
	amount uint64,
	feeAmount int64,
	price decimal.Decimal,
) uint64 {
	feePercentage := decimal.NewFromInt(feeAmount).Div(decimal.NewFromInt(100))
	amountP := decimal.NewFromInt(int64(amount))

	// amountR = amountP + price * (1 - feePercentage)
	amountR := amountP.Mul(price).Mul(decimal.NewFromInt(1).Sub(feePercentage))
	return amountR.BigInt().Uint64()
}

func previewFromFormula(
	unspents []domain.Unspent,
	market *domain.Market,
	tradeType int,
	amount uint64,
) (price Price, previewAmount uint64, err error) {
	balances := getBalanceByAsset(unspents)
	baseBalanceAvailable := balances[market.BaseAssetHash()]
	quoteBalanceAvailable := balances[market.QuoteAssetHash()]
	formula := market.Strategy().Formula()

	if tradeType == TradeBuy {
		previewAmount, err = formula.InGivenOut(
			&mm.FormulaOpts{
				BalanceIn:           quoteBalanceAvailable,
				BalanceOut:          baseBalanceAvailable,
				Fee:                 uint64(market.Fee()),
				ChargeFeeOnTheWayIn: market.FeeAsset() == market.BaseAssetHash(),
			},
			amount,
		)
	} else {
		previewAmount, err = formula.OutGivenIn(
			&mm.FormulaOpts{
				BalanceIn:           baseBalanceAvailable,
				BalanceOut:          quoteBalanceAvailable,
				Fee:                 uint64(market.Fee()),
				ChargeFeeOnTheWayIn: market.FeeAsset() == market.QuoteAssetHash(),
			},
			amount,
		)
	}
	if err != nil {
		return
	}

	price = Price{
		BasePrice: formula.SpotPrice(&mm.FormulaOpts{
			BalanceIn:  quoteBalanceAvailable,
			BalanceOut: baseBalanceAvailable,
		}),
		QuotePrice: formula.SpotPrice(&mm.FormulaOpts{
			BalanceIn:  baseBalanceAvailable,
			BalanceOut: quoteBalanceAvailable,
		}),
	}

	return price, previewAmount, nil
}

func isValidTradePrice(swapRequest *pb.SwapRequest, tradeType int, previewAmount uint64) bool {
	amountToCheck := decimal.NewFromInt(int64(swapRequest.GetAmountP()))
	if tradeType == TradeSell {
		amountToCheck = decimal.NewFromInt(int64(swapRequest.GetAmountR()))
	}
	slippage := decimal.NewFromFloat(config.GetFloat(config.PriceSlippageKey))
	expectedAmount := decimal.NewFromInt(int64(previewAmount))
	lowerBound := expectedAmount.Sub(expectedAmount.Mul(slippage))
	upperBound := expectedAmount.Add(expectedAmount.Mul(slippage))

	return amountToCheck.GreaterThanOrEqual(lowerBound) && amountToCheck.LessThanOrEqual(upperBound)
}
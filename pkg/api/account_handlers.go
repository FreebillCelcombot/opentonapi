package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tonkeeper/opentonapi/internal/g"

	"github.com/tonkeeper/opentonapi/pkg/addressbook"
	"github.com/tonkeeper/tongo/code"
	"github.com/tonkeeper/tongo/utils"

	"github.com/tonkeeper/opentonapi/pkg/core"
	"github.com/tonkeeper/opentonapi/pkg/oas"
	"github.com/tonkeeper/tongo"
	"github.com/tonkeeper/tongo/abi"
	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/tlb"
	walletTongo "github.com/tonkeeper/tongo/wallet"
)

func (h *Handler) GetBlockchainRawAccount(ctx context.Context, params oas.GetBlockchainRawAccountParams) (*oas.BlockchainRawAccount, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	rawAccount, err := h.storage.GetRawAccount(ctx, account.ID)
	if errors.Is(err, core.ErrEntityNotFound) {
		return nil, toError(http.StatusNotFound, err)
	}
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	res := convertToRawAccount(rawAccount)
	return &res, nil
}

func (h *Handler) GetAccount(ctx context.Context, params oas.GetAccountParams) (*oas.Account, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	rawAccount, err := h.storage.GetRawAccount(ctx, account.ID)
	if errors.Is(err, core.ErrEntityNotFound) {
		return &oas.Account{
			Address: account.ID.ToRaw(),
			Status:  string(tlb.AccountNone),
		}, nil
	}
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	ab, found := h.addressBook.GetAddressInfoByAddress(account.ID)
	var res oas.Account
	if found {
		res = convertToAccount(rawAccount, &ab, h.state)
	} else {
		res = convertToAccount(rawAccount, nil, h.state)
	}
	return &res, nil
}

func (h *Handler) GetAccounts(ctx context.Context, request oas.OptGetAccountsReq) (*oas.Accounts, error) {
	if len(request.Value.AccountIds) == 0 {
		return nil, toError(http.StatusBadRequest, fmt.Errorf("empty list of ids"))
	}
	if !h.limits.isBulkQuantityAllowed(len(request.Value.AccountIds)) {
		return nil, toError(http.StatusBadRequest, fmt.Errorf("the maximum number of accounts to request at once: %v", h.limits.BulkLimits))
	}
	var ids []tongo.AccountID
	allAccountIDs := make(map[tongo.AccountID]struct{}, len(request.Value.AccountIds))
	for _, str := range request.Value.AccountIds {
		account, err := tongo.ParseAddress(str)
		if err != nil {
			return nil, toError(http.StatusBadRequest, err)
		}
		ids = append(ids, account.ID)
		allAccountIDs[account.ID] = struct{}{}
	}
	accounts, err := h.storage.GetRawAccounts(ctx, ids)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	results := make([]oas.Account, 0, len(accounts))
	for _, account := range accounts {
		delete(allAccountIDs, account.AccountAddress)
		ab, found := h.addressBook.GetAddressInfoByAddress(account.AccountAddress)
		var res oas.Account
		if found {
			res = convertToAccount(account, &ab, h.state)
		} else {
			res = convertToAccount(account, nil, h.state)
		}
		results = append(results, res)
	}
	// if we don't find an account, we return it with "nonexist" status
	for accountID := range allAccountIDs {
		account := oas.Account{
			Address: accountID.ToRaw(),
			Status:  string(tlb.AccountNone),
		}
		results = append(results, account)
	}
	return &oas.Accounts{Accounts: results}, nil
}

func (h *Handler) GetBlockchainAccountTransactions(ctx context.Context, params oas.GetBlockchainAccountTransactionsParams) (*oas.Transactions, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	if params.BeforeLt.Value == 0 {
		params.BeforeLt.Value = 1 << 62
	}
	txs, err := h.storage.GetAccountTransactions(ctx, account.ID, int(params.Limit.Value), uint64(params.BeforeLt.Value), uint64(params.AfterLt.Value))
	if errors.Is(err, core.ErrEntityNotFound) {
		return &oas.Transactions{}, nil
	}
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	result := oas.Transactions{
		Transactions: make([]oas.Transaction, len(txs)),
	}
	for i, tx := range txs {
		result.Transactions[i] = convertTransaction(*tx, h.addressBook)
	}
	return &result, nil
}

func (h *Handler) ExecGetMethodForBlockchainAccount(ctx context.Context, params oas.ExecGetMethodForBlockchainAccountParams) (*oas.MethodExecutionResult, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	var stack tlb.VmStack
	for _, p := range params.Args {
		r, err := stringToTVMStackRecord(p)
		if err != nil {
			return nil, toError(http.StatusBadRequest, fmt.Errorf("can't parse arg '%v' as any TVMStackValue", p))
		}
		stack = append(stack, r)
	}
	exitCode, stack, err := h.executor.RunSmcMethodByID(ctx, account.ID, utils.MethodIdFromName(params.MethodName), stack)
	if err != nil {
		if errors.Is(err, core.ErrEntityNotFound) {
			return nil, toError(http.StatusNotFound, err)
		}
		return nil, toError(http.StatusInternalServerError, err)
	}
	result := oas.MethodExecutionResult{
		Success:  exitCode == 0 || exitCode == 1,
		ExitCode: int(exitCode),
		Stack:    make([]oas.TvmStackRecord, 0, len(stack)),
	}
	for i := range stack {
		value, err := convertTvmStackValue(stack[i])
		if err != nil {
			return nil, toError(http.StatusInternalServerError, err)
		}
		result.Stack = append(result.Stack, value)
	}
	for _, decoder := range abi.KnownGetMethodsDecoder[params.MethodName] {
		_, v, err := decoder(stack)
		if err == nil {
			value, err := json.Marshal(v)
			if err != nil {
				return nil, toError(http.StatusInternalServerError, err)
			}
			result.SetDecoded(g.ChangeJsonKeys(value, g.CamelToSnake))
			break
		}
	}
	return &result, nil
}

func (h *Handler) SearchAccounts(ctx context.Context, params oas.SearchAccountsParams) (*oas.FoundAccounts, error) {
	accounts := h.addressBook.SearchAttachedAccountsByPrefix(params.Name)
	var (
		response           oas.FoundAccounts
		mapOfFoundAccounts = make(map[tongo.AccountID]addressbook.AttachedAccount)
	)
	for _, account := range accounts {
		accountID, err := tongo.ParseAddress(account.Wallet)
		if err != nil {
			continue
		}
		if account.Symbol != "" {
			if h.spamFilter.IsJettonBlacklisted(accountID.ID, account.Symbol) {
				continue
			}
		}
		mapOfFoundAccounts[accountID.ID] = account
	}
	for _, account := range mapOfFoundAccounts {
		response.Addresses = append(response.Addresses, oas.FoundAccountsAddressesItem{
			Address: account.Wallet,
			Name:    account.Name,
			Preview: account.Preview,
		})
	}

	return &response, nil
}

// ReindexAccount updates internal cache for a particular account.
func (h *Handler) ReindexAccount(ctx context.Context, params oas.ReindexAccountParams) error {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return toError(http.StatusBadRequest, err)
	}
	if err = h.storage.ReindexAccount(ctx, account.ID); err != nil {
		return toError(http.StatusInternalServerError, err)
	}
	return nil
}

func (h *Handler) GetAccountDnsExpiring(ctx context.Context, params oas.GetAccountDnsExpiringParams) (*oas.DnsExpiring, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	var period *int
	if params.Period.Set {
		period = &params.Period.Value
	}
	dnsExpiring, err := h.storage.GetDnsExpiring(ctx, account.ID, period)
	if errors.Is(err, core.ErrEntityNotFound) {
		return &oas.DnsExpiring{}, nil
	}
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	accounts := make([]tongo.AccountID, 0, len(dnsExpiring))
	var response oas.DnsExpiring
	if len(dnsExpiring) == 0 {
		return &response, nil
	}
	for _, dns := range dnsExpiring {
		if dns.DnsItem != nil {
			accounts = append(accounts, dns.DnsItem.Address)
		}
	}
	nfts, err := h.storage.GetNFTs(ctx, accounts)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	for _, dns := range dnsExpiring {
		dei := oas.DnsExpiringItemsItem{
			Name:       dns.Name,
			ExpiringAt: dns.ExpiringAt,
		}
		if dns.DnsItem != nil {
			for _, n := range nfts {
				if n.Address == dns.DnsItem.Address {
					dei.DNSItem = oas.NewOptNftItem(convertNFT(ctx, n, h.addressBook, h.metaCache))
					break
				}
			}
		}
		response.Items = append(response.Items, dei)
	}
	sort.Slice(response.Items, func(i, j int) bool {
		return response.Items[i].ExpiringAt > response.Items[j].ExpiringAt
	})

	return &response, nil
}

func (h *Handler) GetAccountPublicKey(ctx context.Context, params oas.GetAccountPublicKeyParams) (*oas.GetAccountPublicKeyOK, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	pubKey, err := h.storage.GetWalletPubKey(ctx, account.ID)
	if err != nil {
		state, err := h.storage.GetRawAccount(ctx, account.ID)
		if err != nil {
			return nil, toError(http.StatusInternalServerError, err)
		}
		pubKey, err = pubkeyFromCodeData(state.Code, state.Data)
		if err != nil {
			return nil, toError(http.StatusInternalServerError, err)
		}
	}
	return &oas.GetAccountPublicKeyOK{PublicKey: hex.EncodeToString(pubKey)}, nil
}

func (h *Handler) GetAccountSubscriptions(ctx context.Context, params oas.GetAccountSubscriptionsParams) (*oas.Subscriptions, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	subscriptions, err := h.storage.GetSubscriptions(ctx, account.ID)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	var response oas.Subscriptions
	for _, subscription := range subscriptions {
		response.Subscriptions = append(response.Subscriptions, oas.Subscription{
			Address:            subscription.AccountID.ToRaw(),
			WalletAddress:      subscription.WalletAccountID.ToRaw(),
			BeneficiaryAddress: subscription.BeneficiaryAccountID.ToRaw(),
			Amount:             subscription.Amount,
			Period:             subscription.Period,
			StartTime:          subscription.StartTime,
			Timeout:            subscription.Timeout,
			LastPaymentTime:    subscription.LastPaymentTime,
			LastRequestTime:    subscription.LastRequestTime,
			SubscriptionID:     subscription.SubscriptionID,
			FailedAttempts:     subscription.FailedAttempts,
		})
	}
	return &response, nil
}

func (h *Handler) GetAccountTraces(ctx context.Context, params oas.GetAccountTracesParams) (*oas.TraceIDs, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	traceIDs, err := h.storage.SearchTraces(ctx, account.ID, params.Limit.Value, nil, nil, nil, false)
	if err != nil && !errors.Is(err, core.ErrEntityNotFound) {
		return nil, toError(http.StatusInternalServerError, err)
	}
	var traces oas.TraceIDs
	for _, traceID := range traceIDs {
		trace, err := h.storage.GetTrace(ctx, traceID)
		if err != nil {
			return nil, toError(http.StatusInternalServerError, err)
		}
		traces.Traces = append(traces.Traces, oas.TraceID{
			ID:    trace.Hash.Hex(),
			Utime: trace.Lt,
		})
	}
	return &traces, nil
}

func (h *Handler) GetAccountDiff(ctx context.Context, params oas.GetAccountDiffParams) (*oas.GetAccountDiffOK, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	balanceChange, err := h.storage.GetAccountDiff(ctx, account.ID, params.StartDate, params.EndDate)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	return &oas.GetAccountDiffOK{BalanceChange: balanceChange}, nil
}

func (h *Handler) GetAccountNftHistory(ctx context.Context, params oas.GetAccountNftHistoryParams) (*oas.AccountEvents, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	traceIDs, err := h.storage.GetAccountNftsHistory(ctx, account.ID, params.Limit, optIntToPointer(params.BeforeLt), optIntToPointer(params.StartDate), optIntToPointer(params.EndDate))
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	events, lastLT, err := h.convertNftHistory(ctx, account.ID, traceIDs, params.AcceptLanguage)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	return &oas.AccountEvents{Events: events, NextFrom: lastLT}, nil
}

func (h *Handler) BlockchainAccountInspect(ctx context.Context, params oas.BlockchainAccountInspectParams) (*oas.BlockchainAccountInspect, error) {
	account, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	rawAccount, err := h.storage.GetRawAccount(ctx, account.ID)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	cells, err := boc.DeserializeBoc(rawAccount.Code)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	if len(cells) != 1 {
		return nil, toError(http.StatusInternalServerError, fmt.Errorf("invalid boc with code"))
	}
	codeHash, err := cells[0].Hash()
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	methods, err := code.ParseContractMethods(rawAccount.Code)
	if err != nil {
		return nil, toError(http.StatusInternalServerError, err)
	}
	resp := oas.BlockchainAccountInspect{
		Code:     hex.EncodeToString(rawAccount.Code),
		CodeHash: hex.EncodeToString(codeHash),
		Compiler: oas.NewOptBlockchainAccountInspectCompiler(oas.BlockchainAccountInspectCompilerFunc),
	}
	for _, methodID := range methods {
		if method, ok := code.Methods[methodID]; ok {
			resp.Methods = append(resp.Methods, oas.BlockchainAccountInspectMethodsItem{
				ID:     methodID,
				Method: string(method),
			})
		}
	}
	return &resp, nil
}

func pubkeyFromCodeData(code, data []byte) ([]byte, error) {
	cells, err := boc.DeserializeBoc(code)
	if err != nil {
		return nil, err
	}
	if len(cells) != 1 {
		return nil, fmt.Errorf("invalid boc with code")
	}
	codeHash, err := cells[0].Hash()
	if err != nil {
		return nil, err
	}
	ver, ok := walletTongo.GetVerByCodeHash([32]byte(codeHash))
	if !ok {
		return nil, fmt.Errorf("unknown wallet version")
	}
	switch ver {
	case walletTongo.V3R1:
		var dataBody walletTongo.DataV3
		cells, err = boc.DeserializeBoc(data)
		if err != nil {
			return nil, err
		}
		err = tlb.Unmarshal(cells[0], &dataBody)
		if err != nil {
			return nil, err
		}
		return dataBody.PublicKey[:], nil
	default:
		return nil, fmt.Errorf("unknown wallet version")
	}
}

func (h *Handler) AddressParse(ctx context.Context, params oas.AddressParseParams) (*oas.AddressParseOK, error) {
	address, err := tongo.ParseAddress(params.AccountID)
	if err != nil {
		return nil, toError(http.StatusBadRequest, err)
	}
	var res oas.AddressParseOK
	res.RawForm = address.ID.ToRaw()
	res.Bounceable.B64url = address.ID.ToHuman(true, false)
	res.NonBounceable.B64url = address.ID.ToHuman(false, false)
	b, _ := base64.URLEncoding.DecodeString(res.Bounceable.B64url)
	res.Bounceable.B64 = base64.StdEncoding.EncodeToString(b)
	b, _ = base64.URLEncoding.DecodeString(res.NonBounceable.B64url)
	res.NonBounceable.B64 = base64.StdEncoding.EncodeToString(b)
	if strings.Contains(params.AccountID, ":") {
		res.GivenType = "raw_form"
	} else if strings.Contains(params.AccountID, ".") {
		res.GivenType = "dns"
	} else if address.Bounce {
		res.GivenType = "friendly_bounceable"
	} else {
		res.GivenType = "friendly_non_bounceable"
	}
	return &res, nil //todo: add testnet_only
}

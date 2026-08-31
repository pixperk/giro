package api

import (
	"encoding/json"
	"math/big"

	"github.com/pixperk/giro/internal/ledger"
	"github.com/pixperk/giro/internal/storage"
)

// the wire types are generated from the contract and the domain types are
// hand written, so they meet here. keeping them separate means the api shape
// can change without disturbing the engine, and vice versa.

func toAPITransaction(t *ledger.Transaction) Transaction {
	out := Transaction{
		Id:         t.ID,
		Postings:   toAPIPostings(t.Postings),
		Timestamp:  t.Timestamp,
		InsertedAt: t.InsertedAt,
		RevertedAt: t.RevertedAt,
	}
	if t.Reference != "" {
		out.Reference = &t.Reference
	}
	if len(t.Metadata) > 0 {
		m := Metadata(t.Metadata)
		out.Metadata = &m
	}
	if len(t.PostCommitVolumes) > 0 {
		pcv := map[string]map[string]Volumes{}
		for account, byAsset := range t.PostCommitVolumes {
			pcv[account] = map[string]Volumes{}
			for asset, v := range byAsset {
				pcv[account][asset] = toAPIVolumes(v)
			}
		}
		out.PostCommitVolumes = &pcv
	}
	return out
}

func toAPIPostings(p ledger.Postings) []Posting {
	out := make([]Posting, len(p))
	for i, posting := range p {
		out[i] = Posting{
			Source:      posting.Source,
			Destination: posting.Destination,
			Asset:       posting.Asset,
			Amount:      posting.Amount,
		}
	}
	return out
}

func fromAPIPostings(p []Posting) ledger.Postings {
	out := make(ledger.Postings, len(p))
	for i, posting := range p {
		// copied rather than aliased: a caller's slice must not stay reachable
		// from a transaction the ledger has committed.
		amount := new(big.Int).Set(posting.Amount)
		out[i] = ledger.Posting{
			Source:      posting.Source,
			Destination: posting.Destination,
			Asset:       posting.Asset,
			Amount:      amount,
		}
	}
	return out
}

func toAPIVolumes(v ledger.Volumes) Volumes {
	return Volumes{Input: v.Input, Output: v.Output}
}

func toAPIAccount(a *ledger.Account) Account {
	out := Account{
		Address:       a.Address,
		FirstUsage:    a.FirstUsage,
		InsertionDate: a.InsertionDate,
		UpdatedAt:     a.UpdatedAt,
	}
	if len(a.Metadata) > 0 {
		m := Metadata(a.Metadata)
		out.Metadata = &m
	}
	if a.Volumes != nil {
		v := volumesMap(a.Volumes)
		out.Volumes = &v
	}
	if a.EffectiveVolumes != nil {
		v := volumesMap(a.EffectiveVolumes)
		out.EffectiveVolumes = &v
	}
	return out
}

func volumesMap(in map[string]ledger.Volumes) map[string]Volumes {
	out := make(map[string]Volumes, len(in))
	for asset, v := range in {
		out[asset] = toAPIVolumes(v)
	}
	return out
}

func toAPIBalances(b map[string]*big.Int) Balances {
	out := Balances{}
	for asset, amount := range b {
		out[asset] = amount
	}
	return out
}

func toAPILog(l ledger.Log) Log {
	out := Log{
		Id:   l.ID,
		Type: LogType(l.Type),
		Date: l.Date,
		Hash: l.Hash,
		// raw, so the bytes the hash covers are the bytes the client sees
		Data: json.RawMessage(l.Data),
	}
	if l.IdempotencyKey != "" {
		out.IdempotencyKey = &l.IdempotencyKey
	}
	return out
}

func toAPILedger(l *storage.LedgerInfo) Ledger {
	return Ledger{Name: l.Name, AddedAt: l.AddedAt}
}

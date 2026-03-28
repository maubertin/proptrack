package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/models"
)

// CheckVesselSanctions queries Dgraph for existing sanction entries linked
// to the vessel identified by IMO and returns them.
func CheckVesselSanctions(ctx context.Context, imo string) ([]models.SanctionEntry, error) {
	q := `
query checkVessel($imo: string) {
  vessel(func: eq(vessel.imo, $imo)) {
    uid
    vessel.imo
    vessel.sanctioned
    vessel.sanction_programs
    ~sanction.entity_ref {
      uid
      sanction.list
      sanction.entity_type
      sanction.date_listed
      sanction.program
      sanction.notes
    }
  }
}`

	txn := db.NewReadOnlyTxn()
	defer txn.Discard(ctx)

	resp, err := txn.QueryWithVars(ctx, q, map[string]string{"$imo": imo})
	if err != nil {
		return nil, fmt.Errorf("sanctions query: %w", err)
	}

	var result struct {
		Vessel []struct {
			Entries []models.SanctionEntry `json:"~sanction.entity_ref"`
		} `json:"vessel"`
	}
	if err := json.Unmarshal(resp.Json, &result); err != nil {
		return nil, fmt.Errorf("unmarshal sanctions: %w", err)
	}
	if len(result.Vessel) == 0 {
		return nil, nil
	}
	return result.Vessel[0].Entries, nil
}

// UpsertSanctionEntry creates or updates a SanctionEntry node linked to a vessel or company UID.
func UpsertSanctionEntry(ctx context.Context, entry models.SanctionEntry, entityUID string) error {
	type nquad struct {
		UID         string    `json:"uid"`
		DgraphType  []string  `json:"dgraph.type"`
		List        string    `json:"sanction.list"`
		EntityType  string    `json:"sanction.entity_type"`
		EntityRef   string    `json:"sanction.entity_ref"`
		DateListed  time.Time `json:"sanction.date_listed"`
		Program     string    `json:"sanction.program"`
		Notes       string    `json:"sanction.notes"`
	}

	node := nquad{
		UID:        "_:sanction",
		DgraphType: []string{"SanctionEntry"},
		List:       entry.List,
		EntityType: entry.EntityType,
		EntityRef:  entityUID,
		DateListed: entry.DateListed,
		Program:    entry.Program,
		Notes:      entry.Notes,
	}

	b, err := json.Marshal(node)
	if err != nil {
		return err
	}

	txn := db.NewTxn()
	defer txn.Discard(ctx)

	mu := &api.Mutation{SetJson: b, CommitNow: true}
	_, err = txn.Mutate(ctx, mu)
	if err != nil {
		return fmt.Errorf("upsert sanction entry: %w", err)
	}

	slog.Info("sanction entry upserted",
		"list", entry.List,
		"program", entry.Program,
		"entity_uid", entityUID,
	)
	return nil
}

// IsIRISLAffiliated returns true when the company name contains IRISL keywords
// or is tagged as sanctioned under the IRAN program.
func IsIRISLAffiliated(company *models.Company) bool {
	if company == nil {
		return false
	}
	cn := strings.ToLower(company.Name)
	for _, kw := range irisOperatorKeywords {
		if strings.Contains(cn, kw) {
			return true
		}
	}
	aliases := strings.ToLower(company.KnownAliases)
	for _, kw := range irisOperatorKeywords {
		if strings.Contains(aliases, kw) {
			return true
		}
	}
	return false
}

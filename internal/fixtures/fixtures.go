// Package fixtures parses fixtures/<case>/principles.json and claims.jsonl.
// It only parses: posting them to tk-api and translating `ref` placeholders
// to real claim_id values is the importer's job (cmd/tk import; exercised
// here by tests against the real REST API).
package fixtures

import (
	"bufio"
	"encoding/json"
	"os"
)

type Principle struct {
	Code      string `json:"code"`
	Statement string `json:"statement"`
	Signer    string `json:"signer"`
}

// RawBasis mirrors ledger.Basis except Refs holds the fixture file's
// temporary `ref` labels (e.g. "102"), not real claim_id values yet.
type RawBasis struct {
	Kind       string   `json:"kind"`
	Refs       []string `json:"refs,omitempty"`
	Note       string   `json:"note,omitempty"`
	ValidUntil *string  `json:"valid_until,omitempty"`
}

type Claim struct {
	Ref       string    `json:"ref"`
	Statement string    `json:"statement"`
	Mark      string    `json:"mark"`
	Basis     *RawBasis `json:"basis,omitempty"`
	Signer    *string   `json:"signer,omitempty"`
	Producer  string    `json:"producer"`
}

func LoadPrinciples(path string) ([]Principle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Principle
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func LoadClaims(path string) ([]Claim, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Claim
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}
		var c Claim
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, scanner.Err()
}

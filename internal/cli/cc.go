package cli

import "github.com/ankayma/tknowledge-personal/internal/ccclient"

// CauseChannel issues the owner's cause before a move is written.
// From the CLI: approver = producer = signer (the person typing), actor tk-cli.
type CauseChannel = ccclient.Client

func NewCauseChannel(baseURL string) *CauseChannel { return ccclient.New(baseURL) }

const causeActor = "tk-cli"

package demo_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/demo"
	"github.com/ankayma/tknowledge-personal/internal/mcpserver"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// The scenario a stranger is asked to walk, run end to end over the MCP wire
// against a real store. If this fails, nobody should be invited to try.
func TestScenario_TenTurns(t *testing.T) {
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	mcp := httptest.NewServer(mcpserver.HTTPHandler(
		mcpserver.New(tkclient.New(env.Server.URL), ccclient.New(cc.URL))))
	t.Cleanup(mcp.Close)

	var log strings.Builder
	err := demo.Run(context.Background(), demo.Options{
		BaseURL: mcp.URL,
		Grant:   env.Token(t, "demo-user", "claude"),
		Out:     &log,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for _, want := range []string{"USE_PROMOTE", "SOURCE_NOT_RETRIEVABLE", "D-1", "needs_review", "PREMISE_NOT_T"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the walk should have shown %s:\n%s", want, log.String())
		}
	}
}

package transport

import (
	"context"
	"testing"

	"github.com/jeffrom/logd/protocol"
	"github.com/jeffrom/logd/testhelper"
)

func TestMockRequestHandler(t *testing.T) {
	conf := testhelper.DefaultConfig(testing.Verbose())
	rh := NewMockRequestHandler(conf)
	resp := protocol.NewResponseConfig(conf)
	rh.Respond(func(req *protocol.Request) *protocol.Response {
		return resp
	})

	ctx := context.Background()
	req := protocol.NewRequestConfig(conf)
	req.Name = protocol.CmdBatch
	nextResp, err := rh.PushRequest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	if nextResp != resp {
		t.Fatal("responses weren't the same")
	}

	nextResp, err = rh.PushRequest(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	if nextResp != resp {
		t.Fatal("responses weren't the same")
	}
}

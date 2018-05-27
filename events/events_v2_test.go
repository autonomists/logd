package events

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/logger"
	"github.com/jeffrom/logd/protocol"
	"github.com/jeffrom/logd/testhelper"
)

func TestEventQBatchV2(t *testing.T) {
	conf := testhelper.DefaultTestConfig(testing.Verbose())
	q := NewEventQ(conf)
	mw := logger.NewMockWriter(conf)
	q.logw = mw
	if err := q.Start(); err != nil {
		t.Fatalf("%+v", err)
	}

	ctx := context.Background()
	fixture := testhelper.LoadFixture("batch.small")
	req := newRequest(t, conf, fixture)

	resp, err := q.PushRequest(ctx, req)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	checkBatchResp(t, conf, resp)

	parts := mw.Partitions()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 log partition but got %d", len(parts))
	}

	actual := parts[0].Bytes()
	testhelper.CheckGoldenFile("batch.small", actual, testhelper.Golden)

	n, interval := partitionIterations(conf, len(fixture))

	q.logw = logger.NewDiscardWriter(conf)

	for i := 0; i < n; i += interval {
		resp, err = q.PushRequest(ctx, req)
		if err != nil {
			t.Fatalf("%+v", err)
		}
		cr := checkBatchResp(t, conf, resp)
		expectedOffset := calcOffset(len(fixture), i, interval)
		if cr.Offset() != expectedOffset {
			t.Fatalf("expected next message offset to be %d, but was %d", expectedOffset, cr.Offset())
		}
	}
}

func TestQLifecycleV2(t *testing.T) {
	// mockWriter can't keep track of partitions, do this later
	t.SkipNow()
	conf := testhelper.DefaultTestConfig(testing.Verbose())
	q := NewEventQ(conf)
	mw := logger.NewMockWriter(conf)
	q.logw = mw
	if err := q.Start(); err != nil {
		t.Fatalf("%+v", err)
	}

	fixture := testhelper.LoadFixture("batch.small")

	n, interval := partitionIterations(conf, len(fixture))

	x := 1
	for i := 0; i < n; i += interval {
		ctx := context.Background()
		req := newRequest(t, conf, fixture)

		resp, err := q.PushRequest(ctx, req)
		if err != nil {
			t.Fatalf("%+v", err)
		}

		cr := checkBatchResp(t, conf, resp)
		fmt.Println(cr)

		testhelper.CheckError(q.Stop())
		testhelper.CheckError(q.Start())

		expected := uint64(len(fixture) * x)
		actual := q.parts.headOffset()
		if actual != expected {
			t.Fatalf("expected head offset to be %d but was %d", expected, actual)
		}
		x++
	}
}

func newRequest(t testing.TB, conf *config.Config, p []byte) *protocol.Request {
	req := protocol.NewRequest(conf)

	_, err := req.ReadFrom(bufio.NewReader(bytes.NewBuffer(p)))
	if err != nil {
		t.Fatalf("%+v", err)
	}
	return req
}

func checkBatchResp(t testing.TB, conf *config.Config, resp *protocol.ResponseV2) *protocol.ClientResponse {
	if resp.NumReaders() != 1 {
		t.Fatalf("expected 1 reader but got %d", resp.NumReaders())
	}

	r, err := resp.ScanReader()
	if err != nil {
		t.Fatalf("%+v", err)
	}

	cr := protocol.NewClientResponse(conf)
	if _, rerr := cr.ReadFrom(bufio.NewReader(r)); rerr != nil {
		t.Fatalf("%+v", rerr)
	}
	return cr
}

func partitionIterations(conf *config.Config, fixtureLen int) (int, int) {
	n := (conf.PartitionSize / fixtureLen) * (conf.MaxPartitions + 5)
	interval := 1
	if testing.Short() {
		n = 2
		interval = 1
	}
	return n, interval
}

func calcOffset(l, i, interval int) uint64 {
	return uint64(l * ((i / interval) + 1))
}

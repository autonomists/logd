package protocol

import (
	"bytes"
	"testing"

	"github.com/jeffrom/logd/testhelper"
)

func TestWriteTailV2(t *testing.T) {
	conf := testhelper.TestConfig(testing.Verbose())
	tail := NewTail(conf)
	tail.Messages = 100

	b := &bytes.Buffer{}
	if _, err := tail.WriteTo(b); err != nil {
		t.Fatalf("unexpected error writing READ request: %v", err)
	}

	testhelper.CheckGoldenFile("tail.simple", b.Bytes(), testhelper.Golden)
}

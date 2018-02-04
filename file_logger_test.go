package logd

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"runtime/debug"
	"testing"
)

const logDirPrefix = "__logd-testdata__"

// var randSrc *rand.Rand

// func init() {
// 	randSrc = rand.New(rand.NewSource(time.Now().UnixNano()))
// }

// func randStr(l int) string {
// 	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
// 	result := make([]byte, l)
// 	for i := range result {
// 		result[i] = chars[randSrc.Intn(len(chars))]
// 	}
// 	return string(result)
// }

func getTempdir() string {
	dir, err := ioutil.TempDir("", logDirPrefix)
	if err != nil {
		panic(fmt.Sprintf("Failed to get tempdir: %+v", err))
	}

	//
	// debugging
	//

	// fmt.Println(dir)
	// ls, err := ioutil.ReadDir(dir)
	// if err != nil {
	// 	panic(err)
	// }
	// for _, f := range ls {
	// 	fmt.Println(f.Name())
	// }

	//
	//
	//

	return dir
}

func tmpLog() string {
	tmpdir := getTempdir()
	return path.Join(tmpdir, "__log")
}

func setupFileLoggerConfig(t testing.TB, config *Config) (*Config, Logger, func()) {
	var logger Logger
	logger = newFileLogger(config)
	config.Logger = newFileLogger(config)

	if err := logger.(logManager).Setup(); err != nil {
		t.Fatalf("error setting up: %+v", err)
	}

	return config, logger, func() {
		// dir := path.Dir(config.LogFile)
		// t.Logf("Deleting %s", dir)
		// if strings.Contains(dir, logDirPrefix) {
		// 	os.RemoveAll(dir)
		// }

		if err := logger.(logManager).Shutdown(); err != nil {
			t.Fatalf("error shutting down: %+v", err)
		}

	}
}

func setupFileLogger(t testing.TB) (*Config, Logger, func()) {
	config := testConfig(nil)
	config.IndexCursorSize = 10
	config.PartitionSize = 2048
	config.LogFile = tmpLog()

	// t.Logf("test logs at: %s", config.LogFile)
	return setupFileLoggerConfig(t, config)
}

func getLogOutput(config *Config, l io.Reader) []byte {
	b, err := ioutil.ReadAll(l)
	if err != nil {
		panic(err)
	}
	return b
}

func TestFileLoggerCreate(t *testing.T) {
	_, _, teardown := setupFileLogger(t)
	defer teardown()
}

func TestFileLoggerWrite(t *testing.T) {
	config, logger, teardown := setupFileLogger(t)
	defer teardown()

	logger.Write([]byte("really cool message\n"))
	logger.Write([]byte("another really cool message\n"))
	logger.Write([]byte("yet another really cool message\n"))
	logger.Write([]byte("finally a last really cool message\n"))

	b, err := ioutil.ReadFile(config.LogFile + ".0")
	if err != nil {
		t.Fatalf("failed reading log: %+v", err)
	}
	checkGoldenFile(t, "file_logger_write", b, golden)
}

func TestFileLoggerReadSeek(t *testing.T) {
	config, logger, teardown := setupFileLogger(t)
	defer teardown()

	fileWriteLog(t, logger, 1, "one")
	fileWriteLog(t, logger, 2, "two")
	fileWriteLog(t, logger, 3, "three")
	fileWriteLog(t, logger, 4, "four")

	if head, err := logger.Head(); err != nil {
		t.Fatalf("failed getting head of log: %+v", err)
	} else if head != 4 {
		t.Fatalf("expected head to be 4 but was %d", head)
	}

	if err := logger.SeekToID(2); err != nil {
		t.Fatalf("unexpected error seeking: %+v", err)
	}

	out := getLogOutput(config, logger)
	checkGoldenFile(t, "file_logger_read_seek", out, golden)
}

func TestFileLoggerReadPartition(t *testing.T) {
	config := testConfig(nil)
	config.IndexCursorSize = 10
	config.PartitionSize = 500
	config.LogFile = path.Join("testdata", "benjamin_lines")
	config, logger, teardown := setupFileLoggerConfig(t, config)
	defer teardown()

	b := removeMetaFromLog(readRange(t, logger, 1, 1))
	if !bytes.Equal(b, benjaminLines[0]) {
		t.Fatalf("expected:\n\n%q\n\ngot:\n\n%q\n", benjaminLines[0], b)
	}
}

func TestFileLoggerIndex(t *testing.T) {
	config, logger, teardown := setupFileLogger(t)
	defer teardown()

	for i := 1; i < 501; i++ {
		n := i
		if n > 2 {
			n /= 2
		}
		if n > 250 {
			n /= 2
		}
		fileWriteLog(t, logger, uint64(i), repeat("word", n))
	}

	out, _ := ioutil.ReadFile(config.indexFileName())
	checkGoldenFile(t, "file_logger_index", out, golden)
}

func TestFilePartitionWrites(t *testing.T) {
	config := testConfig(nil)
	config.IndexCursorSize = 10
	config.PartitionSize = 500
	config.LogFile = tmpLog()
	config, logger, teardown := setupFileLoggerConfig(t, config)
	defer teardown()

	msg := []byte(repeat("A", 50))
	for i := 0; i < 10; i++ {
		truncated := msg[:len(msg)-(10%(i+1))]
		logger.Write(truncated)
	}
	logger.Flush()

	part1, _ := ioutil.ReadFile(config.LogFile + ".0")
	checkGoldenFile(t, "file_partition_write.0", part1, golden)
	part2, _ := ioutil.ReadFile(config.LogFile + ".1")
	checkGoldenFile(t, "file_partition_write.1", part2, golden)
	_, err := ioutil.ReadFile(config.LogFile + ".2")
	if err == nil {
		t.Fatal("Expected no third partition")
	}
}

func TestFilePartitionDeleteOldOnes(t *testing.T) {
	config := testConfig(nil)
	config.IndexCursorSize = 10
	config.PartitionSize = 500
	config.MaxPartitions = 2
	config.LogFile = tmpLog()
	config, logger, teardown := setupFileLoggerConfig(t, config)
	defer teardown()

	msg := []byte(repeat("A", 500))
	for i := 0; i < 10; i++ {
		logger.Write(msg)
	}
	logger.Flush()

	part1, _ := ioutil.ReadFile(config.LogFile + ".9")
	checkGoldenFile(t, "file_partition_delete_old_ones.9", part1, golden)
	part2, _ := ioutil.ReadFile(config.LogFile + ".10")
	checkGoldenFile(t, "file_partition_delete_old_ones.10", part2, golden)
	_, err := ioutil.ReadFile(config.LogFile + ".11")
	if err == nil {
		t.Fatal("Expected no third partition")
	}
}

func repeat(s string, n int) string {
	b := bytes.Buffer{}
	for i := 0; i < n; i++ {
		b.WriteString(s)
		b.WriteString(" ")
	}
	return b.String()
}

func fileWriteLog(t *testing.T, logger Logger, id uint64, body string) (int, error) {
	logger.SetID(id)
	b := newProtocolWriter().writeLogLine(NewMessage(id, []byte(body)))
	n, err := logger.Write(b)
	if err != nil {
		t.Fatalf("unexpectedly failed to write to log: %+v", err)
		return n, err
	}
	return n, err
}

func readRange(t testing.TB, logger Logger, start, end uint64) []byte {
	p, err := logger.Range(1, 1)
	if err != nil {
		t.Logf("%s", debug.Stack())
		t.Fatalf("failed to read range: %+v", err)
	}

	var finalb []byte
	for p.Next() {
		if err := p.Error(); err != nil {
			t.Logf("%s", debug.Stack())
			t.Fatalf("failed to iterate partition range: %+v", err)
		}

		lf := p.LogFile()
		if b, err := ioutil.ReadAll(lf); err != nil {
			t.Logf("%s", debug.Stack())
			t.Fatalf("failed to read partition: %+v", err)
		} else {
			finalb = append(finalb, b...)
		}
	}

	return finalb
}

func removeMetaFromLog(b []byte) []byte {
	end := bytes.SplitN(b, []byte(" "), 4)[3]
	return end[:len(end)-2]
}

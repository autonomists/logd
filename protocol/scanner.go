package protocol

import (
	"bufio"
	"bytes"
	"hash/crc32"
	"io"
	"strconv"

	"github.com/jeffrom/logd/config"
	"github.com/jeffrom/logd/internal"
	"github.com/pkg/errors"
)

// Scanner reads the log protocol. The same protocol is used for both
// the file log and network chunk protocol.
type Scanner struct {
	config       *config.Config
	br           *bufio.Reader
	LastChunkPos int64
	ChunkPos     int64
	chunkEnd     int64
	msg          *Message
	err          error
}

// NewScanner returns a new instance of a buffered protocol scanner.
func NewScanner(conf *config.Config, r io.Reader) *Scanner {
	// TODO maybe pass through bufio.Reader instead of creating a new one if r
	// is a bufio.Reader?
	return &Scanner{
		config: conf,
		br:     bufio.NewReaderSize(r, 1024*8),
	}
}

// Reset resets the scanner to its initial state
func (ps *Scanner) Reset(r io.Reader) {
	ps.br.Reset(r)
	ps.LastChunkPos = 0
	ps.ChunkPos = 0
	ps.chunkEnd = 0
	ps.msg = nil
	ps.err = nil
}

// Scan reads over log data in a loop
func (ps *Scanner) Scan() bool {
	if ps.chunkEnd <= 0 { // need to read chunk envelope
		if err := ps.scanEnvelope(); err != nil && err != errInvalidFirstByte {
			ps.err = err
			return false
		}
	}

	n, msg, err := ps.ReadMessage()
	ps.LastChunkPos = int64(n)
	ps.ChunkPos += int64(n)
	if ps.chunkEnd > 0 && ps.ChunkPos >= ps.chunkEnd {
		internal.Debugf(ps.config, "completed reading %d byte chunk", ps.ChunkPos)
		ps.ChunkPos = 0
		ps.chunkEnd = 0
	}
	ps.err = err

	ps.msg = msg
	return err == nil
}

func (ps *Scanner) ReadMessage() (int, *Message, error) {
	var id uint64
	var body []byte
	var bodylen int64
	var checksum uint64
	var err error
	var read int

	// fmt.Println("reading line")
	line, err := ReadLine(ps.br)
	// fmt.Printf("read: %q (length: %d) (err: %v)\n", line, len(line)+2, err)
	read += len(line)
	read += 2 // \r\n
	if err != nil {
		ps.err = err
		return read, nil, err
	}

	if bytes.Equal(line, []byte("+EOF")) {
		return read, nil, io.EOF
	}

	parts := bytes.SplitN(line, []byte(" "), 4)
	if len(parts) != 4 {
		// fmt.Printf("%q\n", parts)
		return read, nil, errInvalidProtocolLine
	}

	if id, err = strconv.ParseUint(string(parts[0]), 10, 64); err != nil {
		return read, nil, errors.Wrap(err, "scanning id failed")
	}

	if bodylen, err = strconv.ParseInt(string(parts[1]), 10, 64); err != nil {
		return read, nil, errors.Wrap(err, "scanning body length failed")
	}

	if checksum, err = strconv.ParseUint(string(parts[2]), 10, 32); err != nil {
		return read, nil, errors.Wrap(err, "failed to scan crc")
	}

	body = parts[3]
	// fmt.Printf("%q\n", body)
	if int(bodylen) != len(body) {
		return read, nil, errInvalidBodyLength
	}

	if crc32.Checksum(body, crcTable) != uint32(checksum) {
		return read, nil, errCrcChecksumMismatch
	}

	return read, NewMessage(id, bytes.TrimRight(body, "\r\n")), err
}

func (ps *Scanner) scanEnvelope() error {
	if b, err := ps.br.Peek(1); err != nil {
		if err == io.EOF {
			return err
		}
		return errors.Wrap(err, "failed reading first byte")
	} else if b[0] != '+' {
		return errInvalidFirstByte
	}
	ps.br.ReadByte()
	internal.Debugf(ps.config, "scanning envelope")

	line, err := ReadLine(ps.br)
	if err != nil {
		return err
	}

	if bytes.Equal(line, []byte("EOF")) {
		return io.EOF
	}

	n, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		return errors.Wrap(err, "failed to parse chunk length")
	}
	ps.chunkEnd = n

	internal.Debugf(ps.config, "scanned chunk envelope for %d bytes", n)
	return nil
}

// Message returns the message of the current iteration
func (ps *Scanner) Message() *Message {
	return ps.msg
}

func (ps *Scanner) Error() error {
	return ps.err
}

package disk

import (
	"time"
)

const (
	cursorFile = "cursor.dmp"

	defaultSegmentSize = 10 << 20
	maxBlockSize       = 1 << 20

	defaultPurgeInterval = time.Minute * 10
	defaultMaxAge        = time.Hour * 24 * 7
	initialBackoff       = time.Millisecond * 200
	maxBackoff           = time.Second * 31
	defaultMaxRetries    = 8
	pollEofSleep         = time.Second
	dumpPerBlocks        = 100
)

var (
	currentMagic = [2]byte{0, 0}
)

package byterange

import (
	"context"
	"github.com/gnasnik/titan-sdk-go/titan"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"io"
)

var log = logging.Logger("range")

var (
	// concurrent limits the maximum number of concurrent HTTP requests allowed at the same time.
	concurrent int64 = 10 // TODO: make this configurable

	// rangeSize specifies the maximum size of each file range that can be downloaded in a single HTTP request.
	// Each range of data is read into memory and then written to the output stream, so the amount of memory used is
	// directly proportional to the size of rangeSize.
	//
	// Specifically, the estimated amount of memory used can be calculated as concurrent x rangeSize.
	// Keep an eye on memory usage when modifying this value, as setting it too high can result in excessive memory usage and potential out-of-memory errors.
	rangeSize int64 = 10 << 20 // 10 MiB
)

type Range struct {
	titan *titan.Service
}

func New(service *titan.Service) *Range {
	return &Range{
		titan: service,
	}
}

func (r *Range) GetFile(ctx context.Context, cid cid.Cid) (int64, io.ReadCloser, error) {
	var (
		start int64
		size  int64 = 1 << 10 // 1 KiB
	)

	fileSize, _, err := r.titan.GetRange(ctx, cid, start, size)
	if err != nil {
		log.Errorf("get range failed: %v", err)
		return 0, nil, err
	}

	reader, writer := io.Pipe()

	(&dispatcher{
		cid:        cid,
		fileSize:   fileSize,
		rangeSize:  rangeSize,
		concurrent: int(concurrent),
		titan:      r.titan,
		reader:     reader,
		writer:     writer,
		workers:    make(chan worker, concurrent),
		resp:       make([]chan []byte, 0),
	}).run(ctx)

	return fileSize, reader, nil
}

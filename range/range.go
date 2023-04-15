package _range

import (
	"context"
	"fmt"
	"github.com/gnasnik/titan-sdk-go/titan"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"io"
)

var log = logging.Logger("range")

// TODO: make this configurable
var maxConcurrent int64 = 10

type Range struct {
	titan *titan.Service
}

func New(service *titan.Service) *Range {
	return &Range{
		titan: service,
	}
}

func (r *Range) GetFile(ctx context.Context, cid cid.Cid) (int64, io.ReadCloser, error) {
	var start, end int64
	var defaultRangeSize int64 = 1 << 10 // 1 KiB

	fileSize, data, err := r.titan.GetRange(ctx, cid, start, defaultRangeSize)
	if err != nil {
		log.Errorf("get range: %v", err)
		return 0, nil, err
	}

	size := fileSize / maxConcurrent
	response := make([]chan []byte, maxConcurrent)
	reader, writer := io.Pipe()

	// var round int64
	//maxSize := int64(10 << 20) // 10 MiB
	//if size > maxSize {
	//	size = maxSize
	//}
	//
	//round = int64(math.Ceil(float64(fileSize) / float64(size*maxConcurrent)))
	//
	//fmt.Println("=============round", round)

	for i := 0; i < int(maxConcurrent); i++ {
		start, end = end, end+size
		if end > fileSize {
			end = fileSize
		}

		resp := make(chan []byte)
		go func(start, end int64, resp chan []byte) {
			fmt.Println("start", start, "end", end)
			fileSize, data, err = r.titan.GetRange(ctx, cid, start, end)
			if err != nil {
				log.Errorf("get byte range: %v", err)
				return
			}

			resp <- data
		}(start, end, resp)

		response[i] = resp

	}

	go func() {
		for i := 0; i < len(response); i++ {
			_, err = writer.Write(<-response[i])
			if err != nil {
				log.Errorf("write data: %v", err)
				return
			}
		}

		if err = reader.Close(); err != nil {
			log.Errorf("close reader: %v", err)
			return
		}

	}()

	return fileSize, reader, nil
}

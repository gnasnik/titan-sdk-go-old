package titan

import (
	"context"
	"github.com/gnasnik/titan-sdk-go/config"
	"github.com/gnasnik/titan-sdk-go/merkledag"
	"github.com/gnasnik/titan-sdk-go/notify"
	byteRange "github.com/gnasnik/titan-sdk-go/range"
	"github.com/gnasnik/titan-sdk-go/titan"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-ipfs-files"
	ipld "github.com/ipfs/go-ipld-format"
	unixfile "github.com/ipfs/go-unixfs/file"
	"github.com/pkg/errors"
	"io"
)

type API interface {
	// GetFile get a raw file from the Titan network.
	// The file is downloaded in chunks and assembled locally.
	GetFile(ctx context.Context, cid string) (int64, io.ReadCloser, error)
}

type Client struct {
	config config.Config
	titan  *titan.Service
	dag    ipld.DAGService
	notify *notify.Notification
}

func New(opts ...config.Option) (*Client, error) {
	options := config.DefaultOption()

	for _, opt := range opts {
		opt(&options)
	}

	s, err := titan.New(options)
	if err != nil {
		return nil, err
	}

	c := &Client{
		config: options,
		titan:  s,
		notify: notify.NewNotification(),
	}

	if options.Mode == config.TraversalModeDFS {
		c.dag = merkledag.NewDAGService(c.titan)
	}

	go c.notify.ListenEndOfFile(context.Background(), c.titan.EndOfFile)

	return c, nil
}

func (c *Client) GetFile(ctx context.Context, id string) (int64, io.ReadCloser, error) {
	switch c.config.Mode {
	case config.TraversalModeDFS:
		return c.getFileByDFS(ctx, id)
	case config.TraversalModeRange:
		return c.getFileByRange(ctx, id)
	default:
		return 0, nil, errors.Errorf("unsupported walk algorithm")
	}
}

func (c *Client) getFileByDFS(ctx context.Context, id string) (int64, io.ReadCloser, error) {
	cid, err := cid.Decode(id)
	if err != nil {
		return 0, nil, err
	}

	merkleNode, err := c.dag.Get(ctx, cid)
	if err != nil {
		return 0, nil, err
	}

	node, err := unixfile.NewUnixfsFile(ctx, c.dag, merkleNode)
	if err != nil {
		return 0, nil, err
	}

	size, err := node.Size()
	if err != nil {
		return 0, nil, err
	}

	switch node.(type) {
	case files.File:
		return size, newFileReader(node, c.notify.NotifyEndOfFile), nil
	case files.Directory:
		return 0, nil, errors.Errorf("the merkle dag is directory")
	default:
		return 0, nil, errors.Errorf("operation not supported")
	}
}

func (c *Client) getFileByRange(ctx context.Context, id string) (int64, io.ReadCloser, error) {
	cid, err := cid.Decode(id)
	if err != nil {
		return 0, nil, err
	}

	return byteRange.New(c.titan).GetFile(ctx, cid)
}

var _ API = (*Client)(nil)

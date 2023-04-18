package titan

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gnasnik/titan-sdk-go/config"
	"github.com/gnasnik/titan-sdk-go/internal/codec"
	"github.com/gnasnik/titan-sdk-go/internal/request"
	"github.com/gnasnik/titan-sdk-go/types"
	"github.com/gorilla/mux"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"github.com/pkg/errors"
	"github.com/quic-go/quic-go/http3"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	defaultTimout = 3 * time.Second

	formatRaw = "raw"
	formatCAR = "car"
)

var log = logging.Logger("service")

type Service struct {
	baseAPI         string
	token           string
	httpClient      *http.Client
	conn            net.PacketConn
	natType         types.NATType
	accessibleEdges []*types.Edge

	count   int
	started bool

	clk     sync.Mutex
	clients map[string]*http.Client // holds the connection between user side and edge node

	plk    sync.Mutex
	proofs map[string][]*types.PoWProof
}

type params []interface{}

func New(options config.Config) (*Service, error) {
	if options.Address == "" || options.Token == "" {
		return nil, errors.Errorf("address or token is empty")
	}

	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}

	s := &Service{
		baseAPI:    options.Address + "/rpc/v0",
		token:      options.Token,
		httpClient: defaultHttpClient(conn),
		count:      rand.Int(),
		conn:       conn,
		started:    false,
		clients:    make(map[string]*http.Client),
		proofs:     make(map[string][]*types.PoWProof),
	}

	s.natType, err = s.Discover()
	if err != nil {
		log.Debugf("discover NAT types failed: %v", err)
	}

	go serverHTTP(conn)

	return s, nil
}

func serverHTTP(conn net.PacketConn) {
	handler := mux.NewRouter()
	handler.HandleFunc("/ping", func(writer http.ResponseWriter, h *http.Request) {
		//log.Debugf("receive message from: %s", h.RemoteAddr)
		writer.Write([]byte("pong"))
	})

	tlsConf, err := generateTLSConfig()
	if err != nil {
		log.Errorf("http3 server create TLS configure failed: %v", err)
	}

	(&http3.Server{
		TLSConfig: tlsConf,
		Handler:   handler,
	}).Serve(conn)

}

func (s *Service) selectEdge() (*types.Edge, *http.Client, error) {
	if len(s.accessibleEdges) == 0 {
		return nil, nil, errors.Errorf("no avaliable node")
	}

	luckyEdge := s.roundRobin()

	s.clk.Lock()
	client := s.clients[luckyEdge.NodeID]
	s.clk.Unlock()

	return luckyEdge, client, nil
}

func getData(client *http.Client, edge *types.Edge, namespace string, format string, requestHeader http.Header) (int64, []byte, error) {
	body, err := codec.Encode(edge.Credentials)
	if err != nil {
		return 0, nil, errors.Errorf("send request: %v", err)
	}

	resp, err := request.NewBuilder(client, edge.URL, namespace, requestHeader).
		Option("format", format).
		BodyBytes(body).Get(context.Background())
	if err != nil {
		return 0, nil, errors.Errorf("send request: %v", err)
	}

	defer resp.Close()

	if resp.Error != nil {
		return 0, nil, resp.Error
	}

	data, err := io.ReadAll(resp.Output)
	if err != nil {
		return 0, nil, err
	}

	size := int64(len(data))

	if resp.Header.Get("Content-Range") != "" {
		size, err = getFileSizeFromContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, nil, err
		}
	}

	return size, data, nil
}

func getFileSizeFromContentRange(contentRange string) (int64, error) {
	subs := strings.Split(contentRange, "/")
	if len(subs) != 2 {
		return 0, fmt.Errorf("invalid content range: %s", contentRange)
	}

	return strconv.ParseInt(subs[1], 10, 64)
}

// GetBlock retrieves a raw block from titan http gateway
func (s *Service) GetBlock(ctx context.Context, cid cid.Cid) (blocks.Block, error) {
	err := s.loadEdges(ctx, cid)
	if err != nil {
		return nil, err
	}

	edge, client, err := s.selectEdge()
	if err != nil {
		return nil, err
	}

	start := time.Now()
	namespace := fmt.Sprintf("ipfs/%s", cid.String())
	_, data, err := getData(client, edge, namespace, formatRaw, nil)
	if err != nil {
		return nil, errors.Errorf("post request failed: %v", err)
	}

	proofs := generateProofOfWork(start, edge, int64(len(data)))
	s.plk.Lock()
	if _, ok := s.proofs[edge.SchedulerURL]; !ok {
		s.proofs[edge.SchedulerURL] = make([]*types.PoWProof, 0)
	}
	s.proofs[edge.SchedulerURL] = append(s.proofs[edge.SchedulerURL], proofs)
	s.plk.Unlock()

	return blocks.NewBlock(data), nil
}

// loadEdges retrieves all accessible edge nodes of a file
func (s *Service) loadEdges(ctx context.Context, cid cid.Cid) error {
	if s.started {
		return nil
	}

	s.started = true

	edges, err := s.getEdgeNodesByFile(cid)
	if err != nil {
		return err
	}

	if len(edges) == 0 {
		return errors.Errorf("no edge node found for cid: %s", cid.String())
	}

	return s.filterAccessibleEdges(ctx, edges)
}

// GetRange retrieves specific byte ranges of UnixFS files and raw blocks.
func (s *Service) GetRange(ctx context.Context, cid cid.Cid, start, end int64) (int64, []byte, error) {
	err := s.loadEdges(ctx, cid)
	if err != nil {
		return 0, nil, err
	}

	edge, client, err := s.selectEdge()
	if err != nil {
		return 0, nil, err
	}

	startTime := time.Now()
	namespace := fmt.Sprintf("ipfs/%s", cid.String())
	header := http.Header{}
	header.Add("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	size, data, err := getData(client, edge, namespace, formatCAR, header)
	if err != nil {
		return 0, nil, errors.Errorf("post request failed: %v", err)
	}

	proofs := generateProofOfWork(startTime, edge, int64(len(data)))
	s.plk.Lock()
	if _, ok := s.proofs[edge.SchedulerURL]; !ok {
		s.proofs[edge.SchedulerURL] = make([]*types.PoWProof, 0)
	}
	s.proofs[edge.SchedulerURL] = append(s.proofs[edge.SchedulerURL], proofs)
	s.plk.Unlock()

	return size, data, nil
}

// generateProofOfWork generates a proof of work for a downloaded file.
func generateProofOfWork(start time.Time, edge *types.Edge, size int64) *types.PoWProof {
	end := time.Now()
	cost := time.Since(start)

	return &types.PoWProof{
		StartTime:     start.Unix(),
		EndTime:       end.Unix(),
		DownloadSpeed: size / int64(cost),
		DownloadSize:  size,
		ClientID:      edge.NodeID,
	}
}

func (s *Service) getEdgeNodesByFile(cid cid.Cid) ([]*types.Edge, error) {
	serializedParams, err := json.Marshal(params{cid.String()})
	if err != nil {
		return nil, errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.EdgeDownloadInfos",
		Params:  serializedParams,
	}

	header := http.Header{}
	header.Add("Authorization", "Bearer "+s.token)
	data, err := request.PostJsonRPC(s.httpClient, s.baseAPI, req, header)
	if err != nil {
		return nil, errors.Errorf("post jsonrpc failed: %v", err)
	}

	var list []*types.EdgeDownloadInfoList
	err = json.Unmarshal(data, &list)

	var out []*types.Edge
	for _, item := range list {
		for _, edge := range item.Infos {
			e := &types.Edge{
				NodeID:       edge.NodeID,
				URL:          edge.URL,
				Credentials:  edge.Credentials,
				NATType:      edge.NatType,
				SchedulerURL: item.SchedulerURL,
				SchedulerKey: item.SchedulerKey,
			}
			out = append(out, e)
		}
	}

	return out, err
}

// GetSchedulers get scheduler list in the same region
func (s *Service) GetSchedulers() ([]string, error) {
	serializedParams, err := json.Marshal(params{""})
	if err != nil {
		return nil, errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.GetUserAccessPoint",
		Params:  serializedParams,
	}

	header := http.Header{}
	header.Add("Authorization", "Bearer "+s.token)
	data, err := request.PostJsonRPC(s.httpClient, s.baseAPI, req, header)
	if err != nil {
		return nil, err
	}

	var out types.AccessPoint
	err = json.Unmarshal(data, &out)

	return out.SchedulerURLs, nil
}

// GetPublicAddress return the public address
func (s *Service) GetPublicAddress(schedulerURL string) (types.Host, error) {
	serializedParams, err := json.Marshal(params{})
	if err != nil {
		return types.Host{}, errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.GetExternalAddress",
		Params:  serializedParams,
	}

	data, err := request.PostJsonRPC(s.httpClient, schedulerURL, req, nil)
	if err != nil {
		return types.Host{}, err
	}

	subs := strings.Split(strings.Trim(string(data), "\""), ":")
	if len(subs) != 2 {
		return types.Host{}, errors.Errorf("invalid address: %s", subs)
	}

	return types.Host{
		IP:   subs[0],
		Port: subs[1],
	}, nil
}

// RequestSchedulerToSendPackets sends packet from server side to determine the application connectivity
func (s *Service) RequestSchedulerToSendPackets(remoteAddr string, network, url string) error {
	serializedParams, err := json.Marshal(params{
		network, url,
	})
	if err != nil {
		return errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.CheckNetworkConnectivity",
		Params:  serializedParams,
	}

	_, err = request.PostJsonRPC(s.httpClient, remoteAddr, req, nil)

	return err
}

// EstablishConnectionFromEdge creates a connection from edge node side for the application though the scheduler
func (s *Service) EstablishConnectionFromEdge(edge *types.Edge) error {
	serializedParams, err := json.Marshal(params{edge})
	if err != nil {
		return errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.NatPunch",
		Params:  serializedParams,
	}

	_, err = request.PostJsonRPC(s.httpClient, edge.SchedulerURL, req, nil)

	return err
}

// SendPackets sends packet to the edge node
func (s *Service) SendPackets(remoteAddr string) error {
	serializedParams, err := json.Marshal(params{})
	if err != nil {
		return errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.Version",
		Params:  serializedParams,
	}

	_, err = request.PostJsonRPC(s.httpClient, remoteAddr, req, nil)

	return err
}

// SubmitProofOfWork submits a proof of work for a downloaded file
func (s *Service) SubmitProofOfWork(schedulerAddr string, proofs []*types.PoWProof) error {
	serializedParams, err := json.Marshal(params{proofs})
	if err != nil {
		return errors.Errorf("marshaling params failed: %v", err)
	}

	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.SubmitUserProofsOfWork",
		Params:  serializedParams,
	}

	_, err = request.PostJsonRPC(s.httpClient, schedulerAddr, req, nil)

	return err
}

// roundRobin is a round-robin strategy algorithm for node selection.
func (s *Service) roundRobin() *types.Edge {
	s.clk.Lock()
	defer s.clk.Unlock()

	s.count++
	return s.accessibleEdges[s.count%len(s.accessibleEdges)]
}

func (s *Service) cleanup() {
	s.started = false
	s.accessibleEdges = nil
	s.clients = make(map[string]*http.Client)
	s.proofs = make(map[string][]*types.PoWProof)
}

func (s *Service) EndOfFile() error {
	defer s.cleanup()

	var wg sync.WaitGroup
	wg.Add(len(s.proofs))

	s.plk.Lock()
	for schedulerAddr, proofs := range s.proofs {
		go func(addr string, params []*types.PoWProof) {
			defer wg.Done()

			if err := s.SubmitProofOfWork(addr, params); err != nil {
				log.Errorf("submit proof of work failed: %v", err)
			}
		}(schedulerAddr, proofs)
	}
	s.plk.Unlock()

	wg.Wait()
	return nil
}

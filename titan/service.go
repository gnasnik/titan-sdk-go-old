package titan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gnasnik/titan-sdk-go/config"
	"github.com/gnasnik/titan-sdk-go/internal/codec"
	"github.com/gnasnik/titan-sdk-go/internal/crypto"
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
	"net/url"
	"path"
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
	proofs map[string]*proofParam
}

type proofParam struct {
	Proofs []*types.ProofOfWork
	Key    string
}

type params []interface{}

func New(options config.Config) (*Service, error) {
	if options.Address == "" {
		return nil, errors.Errorf("address or Token is empty")
	}

	conn, err := net.ListenPacket("udp4", options.ListenAddr)
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
		proofs:     make(map[string]*proofParam),
	}

	go serverHTTP(conn)
	go serverTCP(conn)

	return s, nil
}

func serverHTTP(conn net.PacketConn) {
	handler := mux.NewRouter()
	handler.HandleFunc("/ping", func(writer http.ResponseWriter, h *http.Request) {
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

func serverTCP(conn net.PacketConn) {
	srv := &http.Server{
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Debugf("listen tcp on: %s", conn.LocalAddr().String())
	ln, le := net.Listen("tcp", conn.LocalAddr().String())
	if le != nil {
		log.Errorf("tcp listen failed: %v", le)
	}
	srv.Serve(ln)
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
	size, data, err := getData(client, edge, namespace, formatRaw, nil)
	if err != nil {
		return nil, errors.Errorf("post request failed: %v", err)
	}

	proofs := &proofOfWorkParams{
		cid:        cid,
		tStart:     start,
		tEnd:       time.Now(),
		size:       size,
		edge:       edge,
		fileFormat: types.RawFile,
	}

	if err = s.generateProofOfWork(proofs); err != nil {
		return nil, errors.Errorf("generate proof of work failed: %v", err)
	}

	return blocks.NewBlock(data), nil
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
	body, err := codec.Encode(edge.Token)
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

	proofs := &proofOfWorkParams{
		cid:        cid,
		tStart:     startTime,
		tEnd:       time.Now(),
		size:       size,
		edge:       edge,
		rStart:     start,
		rEnd:       end,
		fileFormat: types.CarFile,
	}

	if err = s.generateProofOfWork(proofs); err != nil {
		return 0, nil, errors.Errorf("generate proof of work failed: %v", err)
	}

	return size, data, nil
}

func (s *Service) EdgeSize() int {
	return len(s.accessibleEdges)
}

type proofOfWorkParams struct {
	cid        cid.Cid
	tStart     time.Time
	tEnd       time.Time
	size       int64
	edge       *types.Edge
	rStart     int64
	rEnd       int64
	fileFormat types.FileFormat
}

// generateProofOfWork generates proofs of work for per request.
func (s *Service) generateProofOfWork(params *proofOfWorkParams) error {
	cost := params.tEnd.Sub(params.tStart)
	speed := params.size / int64(cost)
	url := params.edge.SchedulerURL

	s.plk.Lock()
	if _, ok := s.proofs[url]; !ok {
		s.proofs[url] = &proofParam{
			Proofs: make([]*types.ProofOfWork, 0),
			Key:    params.edge.SchedulerKey,
		}
	}

	proofs := &types.ProofOfWork{
		TokenID:    params.edge.Token.ID,
		NodeID:     params.edge.NodeID,
		FileFormat: params.fileFormat,
		Workload: types.Workload{
			CID:           params.cid.String(),
			StartTime:     params.tStart.Unix(),
			EndTime:       params.tEnd.Unix(),
			DownloadSpeed: speed,
			DownloadSize:  params.size,
			Range: &types.FileRange{
				Start: params.rStart,
				End:   params.rEnd,
			},
		},
	}

	s.proofs[url].Proofs = append(s.proofs[url].Proofs, proofs)
	s.plk.Unlock()

	return nil
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
	if s.token != "" {
		header.Add("Authorization", "Bearer "+s.token)
	}
	data, err := request.PostJsonRPC(s.httpClient, s.baseAPI, req, header)
	if err != nil {
		return nil, errors.Errorf("post jsonrpc failed: %v", err)
	}

	var list []*types.EdgeDownloadInfoList
	if err = json.Unmarshal(data, &list); err != nil {
		return nil, err
	}

	var out []*types.Edge
	for _, item := range list {
		for _, edge := range item.Infos {
			e := &types.Edge{
				NodeID:       edge.NodeID,
				URL:          edge.URL,
				Token:        edge.Tk,
				NATType:      edge.NatType,
				SchedulerURL: item.SchedulerURL,
				SchedulerKey: item.SchedulerKey,
			}
			log.Debugf("got edge node: %s, %s", e.URL, e.NATType)
			out = append(out, e)
		}
	}

	log.Debugf("got edge nodes: %d", len(out))

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
	if s.token != "" {
		header.Add("Authorization", "Bearer "+s.token)
	}
	data, err := request.PostJsonRPC(s.httpClient, s.baseAPI, req, header)
	if err != nil {
		return nil, err
	}

	var out types.AccessPoint
	err = json.Unmarshal(data, &out)

	return out.SchedulerURLs, nil
}

// GetCandidates get candidates list in the same region
func (s *Service) GetCandidates(schedulerURL string) ([]string, error) {
	req := request.Request{
		Jsonrpc: "2.0",
		ID:      "1",
		Method:  "titan.GetCandidateURLsForDetectNat",
		Params:  nil,
	}

	data, err := request.PostJsonRPC(s.httpClient, schedulerURL, req, nil)
	if err != nil {
		return nil, err
	}

	var out []string
	err = json.Unmarshal(data, &out)

	return out, nil
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

// RequestCandidateToSendPackets sends packet from server side to determine the application connectivity
func (s *Service) RequestCandidateToSendPackets(remoteAddr string, network, url string) error {
	reqURL := fmt.Sprintf("https://%s/ping", url)
	serializedParams, err := json.Marshal(params{
		network, reqURL,
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
	if err != nil {
		return err
	}

	return err
}

// EstablishConnectionFromEdge creates a connection from edge node side for the application though the scheduler
func (s *Service) EstablishConnectionFromEdge(edge *types.Edge) error {
	serializedParams, err := json.Marshal(params{edge.ToNatPunchReq()})
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
	addr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return err
	}

	_, err = s.conn.WriteTo([]byte("ping"), addr)
	if err != nil {
		return err
	}

	//req := request.Request{
	//	Jsonrpc: "2.0",
	//	ID:      "1",
	//	Method:  "titan.Version",
	//	Params:  nil,
	//}
	//
	//_, err := request.PostJsonRPC(s.httpClient, remoteAddr, req, nil)
	//
	//return err
	return nil
}

// SubmitProofOfWork submits a proof of work for a downloaded file
func (s *Service) SubmitProofOfWork(schedulerAddr string, data []byte) error {
	pushURL, err := getPushURL(schedulerAddr)
	if err != nil {
		return err
	}

	streamReader, err := pushStream(s.httpClient, pushURL, bytes.NewReader(data))
	if err != nil {
		return err
	}

	serializedParams, err := json.Marshal(params{streamReader})
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

func getPushURL(addr string) (string, error) {
	pushURL, err := url.Parse(addr)
	if err != nil {
		return "", err
	}
	switch pushURL.Scheme {
	case "ws":
		pushURL.Scheme = "http"
	case "wss":
		pushURL.Scheme = "https"
	}
	///rpc/v0 -> /rpc/streams/v0/push

	pushURL.Path = path.Join(pushURL.Path, "../streams/v0/push")
	return pushURL.String(), nil
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
	s.proofs = make(map[string]*proofParam)
}

func (s *Service) EndOfFile() error {
	defer s.cleanup()

	var wg sync.WaitGroup
	wg.Add(len(s.proofs))

	s.plk.Lock()
	for schedulerAddr, pp := range s.proofs {
		go func(addr string, params *proofParam) {
			defer wg.Done()

			data, err := encrypt(params.Key, toWorkloadList(params.Proofs))
			if err != nil {
				log.Errorf("encrypting proof failed: %v", err)
				return
			}

			if err := s.SubmitProofOfWork(addr, data); err != nil {
				log.Errorf("submit proof of work failed: %v", err)
			}
		}(schedulerAddr, pp)
	}
	s.plk.Unlock()

	wg.Wait()
	return nil
}

func encrypt(key string, value interface{}) ([]byte, error) {
	data, err := codec.Encode(value)
	if err != nil {
		return nil, err
	}

	pub, err := crypto.DecodePublicKey(key)
	if err != nil {
		return nil, err
	}

	return crypto.Encrypt(data, pub)
}

func toWorkloadList(proofs []*types.ProofOfWork) []*types.WorkloadList {
	workloadInNode := make(map[string]*types.WorkloadList)
	for _, proof := range proofs {
		_, ok := workloadInNode[proof.NodeID]
		if !ok {
			workloadInNode[proof.NodeID] = &types.WorkloadList{
				TokenID:    proof.TokenID,
				ClientID:   proof.ClientID,
				NodeID:     proof.NodeID,
				FileFormat: proof.FileFormat,
				Workloads:  make([]*types.Workload, 0),
			}
		}
		workloadInNode[proof.NodeID].Workloads = append(workloadInNode[proof.NodeID].Workloads, &proof.Workload)
	}

	var out []*types.WorkloadList
	for _, workloadList := range workloadInNode {
		out = append(out, workloadList)
	}

	return out
}

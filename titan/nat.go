package titan

import (
	"context"
	"crypto/tls"
	"github.com/gnasnik/titan-sdk-go/types"
	"github.com/pkg/errors"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"net"
	"net/http"
)

const (
	unknown        = types.NATUnknown
	openInternet   = types.NATOpenInternet
	symmetric      = types.NATSymmetric
	fullCone       = types.NATFullCone
	restricted     = types.NATRestricted
	portRestricted = types.NATPortRestricted
	udpBlock       = types.NATUDPBlock
)

// Discover client-side NAT type discovery
func (s *Service) Discover() (types.NATType, error) {
	schedulers, err := s.GetSchedulers()
	if err != nil {
		return unknown, err
	}

	if len(schedulers) == 0 {
		return unknown, errors.Errorf("can not found scheudler")
	}

	primaryScheduler := schedulers[0]

	// Test I: sends an udp packet to primary scheduler
	publicAddrPrimary, err := s.GetPublicAddress(primaryScheduler)
	if err != nil {
		return udpBlock, err
	}

	log.Infof("PublicAddr: %s", publicAddrPrimary)

	if len(schedulers) < 3 {
		return unknown, errors.Errorf("insufficent schedulers")
	}

	secondaryScheduler := schedulers[1]
	tertiaryScheduler := schedulers[2]

	// Test II: sends an udp packet to secondary scheduler
	publicAddrSecondary, err := s.GetPublicAddress(secondaryScheduler)
	if err != nil {
		return unknown, err
	}

	if publicAddrPrimary.Port != publicAddrSecondary.Port {
		return symmetric, nil
	}

	// Test III: sends a tcp packet to primaryScheduler from tertiary scheduler
	err = s.RequestSchedulerToSendPackets(tertiaryScheduler, "tcp", publicAddrPrimary.String())
	if err == nil {
		return openInternet, nil
	}

	// Test IV: sends an udp packet to primaryScheduler from tertiary scheduler
	err = s.RequestSchedulerToSendPackets(tertiaryScheduler, "udp", publicAddrPrimary.String())
	if err == nil {
		return fullCone, nil
	}

	// Test V: sends an udp packet to primaryScheduler from primary scheduler
	err = s.RequestSchedulerToSendPackets(primaryScheduler, "udp", publicAddrPrimary.String())
	if err == nil {
		return restricted, nil
	}

	return portRestricted, nil
}

// filterAccessibleEdges filtering out the list of available edges to only include those that are accessible by the client
// and added to the list of accessible accessibleEdges.
func (s *Service) filterAccessibleEdges(ctx context.Context, edges []*types.Edge) error {
	for _, edge := range edges {
		client, err := s.determineClient(ctx, s.natType, edge)
		if err != nil {
			log.Errorf("determine client: %v", err)
			continue
		}

		s.clk.Lock()
		s.accessibleEdges = append(s.accessibleEdges, edge)
		s.clients[edge.NodeID] = client
		s.clk.Unlock()

	}
	return nil
}

// determineClient determines that can be directly connected to using the default httpclient.
// If an edge is not directly accessible, attempts NAT traversal to see if the edge can be accessed that way.
// If NAT traversal is successful, the edge is wrapped into a new httpclient.
func (s *Service) determineClient(ctx context.Context, userNATType types.NATType, edge *types.Edge) (*http.Client, error) {
	edgeNATType := edge.GetNATType()

	// Check if the edge is already directly accessible
	if edgeNATType == openInternet || edgeNATType == fullCone {
		return s.httpClient, nil
	}

	// Check if the user has an open Internet NAT type, then try to establish a connection through NAT traversal
	if userNATType == openInternet || userNATType == fullCone || edgeNATType == unknown {
		if err := s.EstablishConnectionFromEdge(edge); err != nil {
			return nil, errors.Errorf("establish connection from edge: %v", err)
		}

		return s.httpClient, nil
	}

	// Check if the edge and the user both have a restricted cone NAT type, then request the scheduler to connect to the edge node
	if edgeNATType == restricted || userNATType == restricted {
		err := s.RequestSchedulerToSendPackets(edge.SchedulerURL, "udp", edge.URL)
		if err != nil {
			return nil, errors.Errorf("request scheduler to send packets: %v", err)
		}

		conn, err := createConnection(ctx, s.conn, edge.URL)
		if err != nil {
			return nil, errors.Errorf("create connection: %v", err)
		}

		return newClient(conn), nil
	}

	// Check if the edge and the user both have a restricted port cone NAT type, then try to send packets to the edge and request the scheduler to do so as well
	if edgeNATType == portRestricted && userNATType == portRestricted {
		err := s.SendPackets(edge.URL)

		err = s.RequestSchedulerToSendPackets(edge.SchedulerURL, "udp", edge.URL)
		if err != nil {
			return nil, errors.Errorf("request scheduler to send packets: %v", err)
		}

		conn, err := createConnection(ctx, s.conn, edge.URL)
		if err != nil {
			return nil, errors.Errorf("create connection: %v", err)
		}

		return newClient(conn), nil
	}

	if edgeNATType == symmetric || userNATType == symmetric {
		// TODO: request the scheduler to send packets and guess the port
		return nil, errors.Errorf("symmetric NAT unimplemented")
	}

	log.Errorf("unknown NAT type: %v", edgeNATType)
	return nil, errors.Errorf("unknown NAT type")
}

func newClient(conn quic.EarlyConnection) *http.Client {
	return &http.Client{Transport: &http3.RoundTripper{
		TLSClientConfig: defaultTLSConf(),
		QuicConfig:      defaultQUICConfig(),
		Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
			return conn, nil
		},
	}}
}

func createConnection(ctx context.Context, conn net.PacketConn, remoteAddr string) (quic.EarlyConnection, error) {
	addr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimout)
	defer cancel()

	return quic.DialEarlyContext(ctx, conn, addr, "localhost", defaultTLSConf(), defaultQUICConfig())
}

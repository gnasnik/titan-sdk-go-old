package types

import (
	"fmt"
	"strconv"
)

type NATType int

const (
	NATUnknown NATType = iota
	NATOpenInternet
	NATSymmetric
	NATFullCone
	NATRestricted
	NATPortRestricted
	NATUDPBlock
	NATUDPFirewall
)

func (n NATType) String() string {
	switch n {
	case NATUnknown:
		return "Unknown"
	case NATFullCone:
		return "FullCone"
	case NATRestricted:
		return "Restricted"
	case NATPortRestricted:
		return "PortRestricted"
	case NATSymmetric:
		return "Symmetric"
	default:
		return ""
	}
}

// GatewayCredentials the ability to access edge node
type GatewayCredentials struct {
	// encrypted AccessToken
	Ciphertext string
	Sign       string
}

type EdgeDownloadInfo struct {
	URL         string
	Credentials *GatewayCredentials
	NodeID      string
	NatType     string
}

type EdgeDownloadInfoList struct {
	Infos        []*EdgeDownloadInfo
	SchedulerURL string
	SchedulerKey string
}

type Edge struct {
	URL          string
	Credentials  *GatewayCredentials
	NodeID       string
	NATType      string
	SchedulerURL string
	SchedulerKey string
}

func (e Edge) GetNATType() NATType {
	t, _ := strconv.ParseInt(e.NATType, 10, 64)
	return NATType(t)
}

type AccessPoint struct {
	AreaID        string
	SchedulerURLs []string
}

type Host struct {
	IP   string
	Port string
}

func (h Host) String() string {
	return fmt.Sprintf("%s:%s", h.IP, h.Port)
}

type PoWProof struct {
	TicketID      string
	ClientID      string
	DownloadSpeed int64
	DownloadSize  int64
	StartTime     int64
	EndTime       int64
}

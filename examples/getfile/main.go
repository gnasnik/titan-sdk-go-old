package main

import (
	"context"
	"github.com/cheggaaa/pb"
	"github.com/gnasnik/titan-sdk-go"
	"github.com/gnasnik/titan-sdk-go/config"
	"io"
	"log"
)

func main() {
	address := "https://127.0.0.1:5000"
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJBbGxvdyI6WyJyZWFkIiwid3JpdGUiLCJzaWduIiwiYWRtaW4iXX0.dRbUYnLDWAvFwqp3J4grGGgQp7pwbFVpWaBoii4NZzU"

	client, err := titan.New(
		config.AddressOption(address),
		config.TokenOption(token),
		config.TraversalModeOption(config.TraversalModeRange),
	)
	if err != nil {
		log.Fatal(err)
	}

	cid := "QmQmbAk3PRdgLPwUDbrDdbiqP23VCVrF1Y5MVYrBXwGZHy"
	size, reader, err := client.GetFile(context.Background(), cid)
	if err != nil {
		log.Fatal(err)
	}

	defer reader.Close()

	bar := pb.New64(size).SetUnits(pb.U_BYTES)
	bar.ShowSpeed = true
	barR := bar.NewProxyReader(reader)

	bar.Start()
	defer bar.Finish()

	io.Copy(io.Discard, barR)
}

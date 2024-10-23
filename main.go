package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/namsral/flag"
)

var (
	version = "dev" // overridden by -ldflags

	dns        string
	hostedZone string
	dnsTTL     int
	ipAddress  string
	setupDelay int

	register, unRegister bool

	r53 *route53.Client
)

func configureFromFlags(ctx context.Context) {
	flag.StringVar(&dns, "dns", "my.example.com", "DNS name to register in Route53")
	flag.StringVar(&hostedZone, "hostedzone", "Z2AAAABCDEFGT4", "Hosted zone ID in route53")
	flag.IntVar(&dnsTTL, "dnsttl", 10, "Timeout for DNS entry")
	flag.StringVar(&ipAddress, "ipaddress", "public-ipv4", "IP Address for A Record")
	flag.BoolVar(&register, "register", false, "Register DNS and exit")
	flag.BoolVar(&unRegister, "unregister", false, "Unregister DNS and exit")
	flag.IntVar(&setupDelay, "setupdelay", 10, "Wait time before setting up DNS (in seconds)")
	flag.Parse()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize aws config: %v", err)
	}

	if ipAddress == "public-ipv4" {
		log.Printf("Fetching IP Address from EC2 public-ipv4")

		client := imds.NewFromConfig(cfg)
		output, err := client.GetMetadata(ctx, &imds.GetMetadataInput{Path: "public-ipv4"})
		if err != nil {
			log.Fatalf("Unable to retrieve the public IPv4 address from the EC2 metadata: %s\n", err)
		}
		publicIpv4, err := io.ReadAll(output.Content)
		if err != nil {
			log.Fatalf("Failed to fetch IPV4 public IP: %v", err)
		}
		ipAddress = string(publicIpv4)
	} else if ipAddress == "ecs" {
		log.Printf("Fetching IP Address from ECS metadata")
		metadata, err := getEcsMetadata()
		if err != nil {
			log.Fatalf("Failed to fetch ECS metadata: %v", err)
		}
		ipAddress = metadata.Networks[0].IPv4Addresses[0] // use the first IP address
		if metadata.DesiredStatus == "STOPPED" {
			log.Fatalf("ECS container is being stopped, exiting")
		}
	}

	r53 = route53.NewFromConfig(cfg)
}

func dumpConfig() {
	log.Printf("Version=%v", version)
	log.Printf("DNS=%v", dns)
	log.Printf("DNSTTL=%v", dnsTTL)
	log.Printf("HOSTEDZONE=%v", hostedZone)
	log.Printf("IPADDRESS=%v", ipAddress)
	log.Infof("SETUPDELAY=%v", setupDelay)
}

func tearDownDNS(ctx context.Context) {
	log.Printf("Tearing down Route 53 DNS Name A %s => %s", dns, ipAddress)
	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionDelete,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(dns),
						ResourceRecords: []types.ResourceRecord{
							{
								Value: aws.String(ipAddress),
							},
						},
						TTL:           aws.Int64(int64(dnsTTL)),
						Type:          types.RRTypeA,
						Weight:        aws.Int64(100),
						SetIdentifier: aws.String(ipAddress),
					},
				},
			},
		},
		HostedZoneId: aws.String(hostedZone),
	}

	changeSet, err := r53.ChangeResourceRecordSets(ctx, input)

	if err != nil {
		log.Fatalf("Failed to delete DNS, exiting: %v", err.Error())
	}

	log.Print("Request sent to Route 53...")
	waitForSync(ctx, changeSet)

	// Then wait the DNS Timeout to expire
	log.Printf("Waiting for DNS Timeout to expire (%d seconds)", dnsTTL)
	time.Sleep(time.Duration(dnsTTL) * time.Second)
	log.Print("DNS Timeout expiry finished")
}

func setupDNS(ctx context.Context) {
	log.Printf("Setting up Route 53 DNS Name A %s => %s", dns, ipAddress)

	// Wait for setupDelay
	if setupDelay > 0 {
		log.Infof("Waiting %d seconds before setting up DNS (SETUPDELAY)", setupDelay)
    time.Sleep(time.Duration(setupDelay) * time.Second)
		log.Info("Finished waiting")
	}

	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(dns),
						ResourceRecords: []types.ResourceRecord{
							{
								Value: aws.String(ipAddress),
							},
						},
						TTL:           aws.Int64(int64(dnsTTL)),
						Type:          types.RRTypeA,
						Weight:        aws.Int64(100),
						SetIdentifier: aws.String(ipAddress),
					},
				},
			},
			Comment: aws.String("route53-sidecar"),
		},
		HostedZoneId: aws.String(hostedZone),
	}

	changeSet, err := r53.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		log.Printf("Failed to create DNS: %v", err.Error())
		return
	}

	log.Print("Request sent to Route 53...")
	waitForSync(ctx, changeSet)
}

func waitForSync(ctx context.Context, changeSet *route53.ChangeResourceRecordSetsOutput) {
	failures := 0
	for {
		if err := SleepWithContext(ctx, 5*time.Second); err != nil {
			log.Print("Context cancelled, stop waiting for Route53 ChangeSet to propogate")
			return
		}

		changeOutput, err := r53.GetChange(ctx, &route53.GetChangeInput{
			Id: changeSet.ChangeInfo.Id,
		})

		if err != nil {
			log.Printf("Failed getting ChangeSet result: %v", err)
			if failures++; failures > 3 {
				log.Fatal("Failed the maximum times getting changeset, exiting")
			}
			continue
		}

		if changeOutput.ChangeInfo.Status == "INSYNC" {
			log.Print("Route53 Change Completed")
			break
		}

		log.Printf("Route53 Change not yet propogated (ChangeInfo.Status = %s)...", changeOutput.ChangeInfo.Status)
	}
}

type ecsMetadata struct {
	DesiredStatus string `json:"DesiredStatus"`
	Networks      []struct {
		IPv4Addresses []string `json:"IPv4Addresses"`
	} `json:"Networks"`
}

func getEcsMetadata() (*ecsMetadata, error) {
	// Get metadata URI from ECS_CONTAINER_METADATA_URI_V4 or ECS_CONTAINER_METADATA_URI
	uri := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if uri == "" {
		uri = os.Getenv("ECS_CONTAINER_METADATA_URI")
	}
	client := http.Client{
		Timeout: 1 * time.Second, // 1 second timeout, same as ec2metadata
	}
	resp, err := client.Get(uri)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	metadata := &ecsMetadata{}
	if err = json.NewDecoder(resp.Body).Decode(metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func SleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configureFromFlags(ctx)
	dumpConfig()

	if register {
		setupDNS(ctx)
	} else if unRegister {
		tearDownDNS(ctx)
	} else { // Setup DNS then teardown when sigterm or sigint is received
		setupDNS(ctx)
		<-ctx.Done()                      // Wait for signal, not calling stop() to make sure we don't get killed during clean up
		tearDownDNS(context.Background()) // Cleanup needs its own context
	}
}

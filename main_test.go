package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/ptr"
)

func Test_getEcsMetadata(t *testing.T) {
	const want = "127.0.0.1"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Name":"curl","Networks":[{"IPv4Addresses":["` + want + `"]}]}`))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	os.Setenv("ECS_CONTAINER_METADATA_URI_V4", server.URL)

	got, err := getEcsMetadata()
	if err != nil {
		t.Errorf("getEcsMetadata() error = %v", err)
		return
	}
	if got.Networks[0].IPv4Addresses[0] != want {
		t.Errorf("getEcsMetadata() = %v, want %v", got, want)
	}
}

type mockRoute53 struct {
	calls    int
	wantErrs []error
}

func (m *mockRoute53) ChangeResourceRecordSets(ctx context.Context, input *route53.ChangeResourceRecordSetsInput, opts ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	var err error
	if m.calls < len(m.wantErrs) {
		err = m.wantErrs[m.calls]
	}
	m.calls++
	return &route53.ChangeResourceRecordSetsOutput{
		ChangeInfo: &types.ChangeInfo{
			Id: aws.String("mockChangeId"),
		},
	}, err
}

func (m *mockRoute53) GetChange(ctx context.Context, input *route53.GetChangeInput, opts ...func(*route53.Options)) (*route53.GetChangeOutput, error) {
	return &route53.GetChangeOutput{
		ChangeInfo: &types.ChangeInfo{
			Status: types.ChangeStatusInsync,
		},
	}, nil
}

func Test_setupDNS(t *testing.T) {
	ctx := context.Background()

	dns = "cname.nextjs.internal."
	ipAddress = "1.2.3.4"

	t.Run("success", func(t *testing.T) {
		r53 = &mockRoute53{}

		if err := setupDNS(ctx); err != nil {
			t.Errorf("setupDNS() error = %v", err)
		}
	})

	t.Run("retries", func(t *testing.T) {
		r53 = &mockRoute53{
			wantErrs: []error{&smithy.OperationError{
				ServiceID:     "Route 53",
				OperationName: "ChangeResourceRecordSets",
				Err: &types.InvalidChangeBatch{
					Message: ptr.String("[RRSet of type A with DNS name cname.nextjs.internal. is not permitted because a conflicting RRSet of type CNAME with the same DNS name already exists in zone nextjs.internal.]"),
				},
			}},
		}
		if err := setupDNS(ctx); err != nil {
			t.Errorf("setupDNS() error = %v", err)
		}
	})
}

package dynamostore

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/storetest"
)

// Set SWS_TEST_DYNAMODB_ENDPOINT to a DynamoDB-compatible endpoint (DynamoDB
// Local, or moto_server) to run the suite. Each test gets its own table.
var tableSeq atomic.Int64

func endpoint(t *testing.T) string {
	e := os.Getenv("SWS_TEST_DYNAMODB_ENDPOINT")
	if e == "" {
		t.Skip("SWS_TEST_DYNAMODB_ENDPOINT not set")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	return e
}

func TestConformance(t *testing.T) {
	ep := endpoint(t)
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		table := fmt.Sprintf("sws-test-%d-%d", time.Now().UnixNano(), tableSeq.Add(1))
		opts.DSN = fmt.Sprintf("dynamodb://%s?region=us-east-1&endpoint=%s&create_table=true", table, ep)
		s, err := Open(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestNativeTTL(t *testing.T) {
	ep := endpoint(t)
	table := fmt.Sprintf("sws-ttl-%d", time.Now().UnixNano())
	opened, err := Open(context.Background(), store.Options{
		DSN:       fmt.Sprintf("dynamodb://%s?region=us-east-1&endpoint=%s&create_table=true", table, ep),
		Retention: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Store)
	ctx := context.Background()
	desc, err := s.db.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(table)})
	if err != nil {
		t.Fatal(err)
	}
	if d := desc.TimeToLiveDescription; d == nil || aws.ToString(d.AttributeName) != ttlAttr || d.TimeToLiveStatus != "ENABLED" {
		t.Errorf("ttl not enabled: %+v", d)
	}

	if err := s.PutLink(ctx, store.Link{Pairing: "eng", SlackTS: "1.1", WebexID: "W1"}); err != nil {
		t.Fatal(err)
	}
	item, _ := s.get(ctx, linkPK("eng", "1.1"), noSort)
	if exp := getN(item, ttlAttr); exp < time.Now().Add(29*24*time.Hour).Unix() {
		t.Errorf("expires = %d, want ~30 days out", exp)
	}
	// Opening again must not fail now that TTL is already on.
	if _, err := New(ctx, s.db, table, false, store.Options{Retention: time.Hour}); err != nil {
		t.Errorf("reopen: %v", err)
	}
}

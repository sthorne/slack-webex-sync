package all

import (
	"reflect"
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/store"
)

func TestAllBackendsRegistered(t *testing.T) {
	want := []string{"dynamodb", "memory", "mongodb", "postgres", "redis", "sqlite"}
	if got := store.Drivers(); !reflect.DeepEqual(got, want) {
		t.Errorf("Drivers() = %v, want %v", got, want)
	}
}

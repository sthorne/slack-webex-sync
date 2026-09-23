package memstore

import (
	"testing"

	"github.com/sthorne/slack-webex-sync/internal/store"
	"github.com/sthorne/slack-webex-sync/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
		return New(opts)
	})
}

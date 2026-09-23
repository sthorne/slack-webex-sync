// Package all registers every built-in store backend. Import it for its
// side effects:
//
//	import _ "github.com/sthorne/slack-webex-sync/internal/store/all"
package all

import (
	_ "github.com/sthorne/slack-webex-sync/internal/store/dynamostore" // "dynamodb"
	_ "github.com/sthorne/slack-webex-sync/internal/store/memstore"    // "memory"
	_ "github.com/sthorne/slack-webex-sync/internal/store/mongostore"  // "mongodb"
	_ "github.com/sthorne/slack-webex-sync/internal/store/redisstore"  // "redis"
	_ "github.com/sthorne/slack-webex-sync/internal/store/sqlstore"    // "sqlite", "postgres"
)

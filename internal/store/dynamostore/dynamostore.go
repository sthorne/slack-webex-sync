// Package dynamostore is the Amazon DynamoDB store backend, registered as
// "dynamodb".
//
// DSN:
//
//	dynamodb://TABLE?region=us-east-1&endpoint=http://localhost:8000&create_table=true
//
// Credentials come from the standard AWS chain (environment variables,
// shared config, or an instance or task role). endpoint is only for local
// testing (DynamoDB Local, moto). With create_table=true, a missing table is
// created as on-demand (PAY_PER_REQUEST). Otherwise the table must already
// exist with a string partition key "pk" and a string sort key "sk".
//
// Everything lives in that one table:
//
//	pk                               sk          item
//	L␟{pairing}␟{ts}                 "-"         link
//	LW␟{webexID}                     "-"         pointer to the link
//	N␟{pairing}␟{ts}␟{user}␟{reaction} "-"      reaction note
//	R␟{activityID}                   "-"         Webex reaction
//	RS␟{webexID}␟{reaction}          activityID  reaction membership (for counting)
//	KV␟{key}                         "-"         setting (never expires)
//
// Expiry uses DynamoDB's native TTL. Each record carries an "expires"
// attribute (epoch seconds, created + retention), and Open enables TTL on
// that attribute. DynamoDB deletes expired items lazily (usually within a
// few days), so reads skip items past their expiry. Purge deletes nothing;
// it only records the cutoff so reads also hide anything older than it.
package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/sthorne/slack-webex-sync/internal/model"
	"github.com/sthorne/slack-webex-sync/internal/store"
)

func init() { store.Register("dynamodb", Open) }

const (
	sep       = "\x1f"
	noSort    = "-"
	ttlAttr   = "expires"
	createdAt = "created"
)

// API is the subset of the DynamoDB client used here.
type API interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTimeToLive(context.Context, *dynamodb.DescribeTimeToLiveInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
	UpdateTimeToLive(context.Context, *dynamodb.UpdateTimeToLiveInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
}

// Store implements store.Store on DynamoDB.
type Store struct {
	db        API
	table     string
	retention time.Duration
	now       func() time.Time

	mu           sync.RWMutex
	purgedBefore int64 // unix micros; records created earlier are hidden
}

// Open parses the DSN, connects, and prepares the table.
func Open(ctx context.Context, opts store.Options) (store.Store, error) {
	u, err := url.Parse(opts.DSN)
	if err != nil || u.Scheme != "dynamodb" || u.Host == "" {
		return nil, fmt.Errorf("dynamodb dsn must look like dynamodb://TABLE?region=..., got %q", opts.DSN)
	}
	q := u.Query()
	var loadOpts []func(*config.LoadOptions) error
	if region := q.Get("region"); region != "" {
		loadOpts = append(loadOpts, config.WithRegion(region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if endpoint := q.Get("endpoint"); endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	create, _ := strconv.ParseBool(q.Get("create_table"))
	return New(ctx, client, u.Host, create, opts)
}

// New prepares a store on an existing client.
func New(ctx context.Context, db API, table string, createTable bool, opts store.Options) (*Store, error) {
	s := &Store{db: db, table: table, retention: opts.Retention, now: opts.Clock()}
	if err := s.ensureTable(ctx, createTable); err != nil {
		return nil, err
	}
	if s.retention > 0 {
		if err := s.ensureTTL(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) ensureTable(ctx context.Context, create bool) error {
	_, err := s.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table})
	var notFound *types.ResourceNotFoundException
	if err == nil || !errors.As(err, &notFound) || !create {
		if err != nil {
			return fmt.Errorf("describe table %s: %w", s.table, err)
		}
		return nil
	}
	_, err = s.db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &s.table,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		return fmt.Errorf("create table %s: %w", s.table, err)
	}
	waiter := dynamodb.NewTableExistsWaiter(s.db)
	return waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: &s.table}, 2*time.Minute)
}

// ensureTTL turns on native expiry for the "expires" attribute.
func (s *Store) ensureTTL(ctx context.Context) error {
	desc, err := s.db.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: &s.table})
	if err != nil {
		return fmt.Errorf("describe ttl: %w", err)
	}
	if d := desc.TimeToLiveDescription; d != nil && d.AttributeName != nil && *d.AttributeName == ttlAttr &&
		(d.TimeToLiveStatus == types.TimeToLiveStatusEnabled || d.TimeToLiveStatus == types.TimeToLiveStatusEnabling) {
		return nil
	}
	_, err = s.db.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               &s.table,
		TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String(ttlAttr), Enabled: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("enable ttl on %s (needs dynamodb:UpdateTimeToLive): %w", s.table, err)
	}
	return nil
}

func (s *Store) Close() error { return nil }

// ---------------------------------------------------------------------------
// item helpers

func pk(parts ...string) string { return strings.Join(parts, sep) }

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func num(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func key(partition, sort string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": str(partition), "sk": str(sort)}
}

func getS(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func getN(item map[string]types.AttributeValue, name string) int64 {
	if v, ok := item[name].(*types.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		return n
	}
	return 0
}

// record builds an item with its creation stamp and, when retention is
// set, its native expiry.
func (s *Store) record(partition, sort string, attrs map[string]types.AttributeValue) map[string]types.AttributeValue {
	now := s.now()
	item := key(partition, sort)
	for k, v := range attrs {
		item[k] = v
	}
	item[createdAt] = num(now.UnixMicro())
	if s.retention > 0 {
		item[ttlAttr] = num(now.Add(s.retention).Unix())
	}
	return item
}

// live reports whether an item exists and has neither expired nor been
// purged.
func (s *Store) live(item map[string]types.AttributeValue) bool {
	if len(item) == 0 {
		return false
	}
	if exp := getN(item, ttlAttr); exp > 0 && exp <= s.now().Unix() {
		return false
	}
	s.mu.RLock()
	cutoff := s.purgedBefore
	s.mu.RUnlock()
	return getN(item, createdAt) >= cutoff
}

func (s *Store) get(ctx context.Context, partition, sort string) (map[string]types.AttributeValue, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: key(partition, sort), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if !s.live(out.Item) {
		return nil, nil
	}
	return out.Item, nil
}

func (s *Store) put(partition, sort string, attrs map[string]types.AttributeValue) types.TransactWriteItem {
	return types.TransactWriteItem{Put: &types.Put{TableName: &s.table, Item: s.record(partition, sort, attrs)}}
}

func (s *Store) del(partition, sort string) types.TransactWriteItem {
	return types.TransactWriteItem{Delete: &types.Delete{TableName: &s.table, Key: key(partition, sort)}}
}

func (s *Store) transact(ctx context.Context, items ...types.TransactWriteItem) error {
	_, err := s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	return err
}

// ---------------------------------------------------------------------------
// Links

func linkPK(pairing, ts string) string { return pk("L", pairing, ts) }
func webexPK(webexID string) string    { return pk("LW", webexID) }

func (s *Store) PutLink(ctx context.Context, l store.Link) error {
	items := []types.TransactWriteItem{
		s.put(linkPK(l.Pairing, l.SlackTS), noSort, map[string]types.AttributeValue{
			"pairing": str(l.Pairing), "slack_ts": str(l.SlackTS), "webex_id": str(l.WebexID),
			"origin": str(string(l.Origin)), "slack_thread_ts": str(l.SlackThreadTS), "webex_parent_id": str(l.WebexParentID),
		}),
		s.put(webexPK(l.WebexID), noSort, map[string]types.AttributeValue{
			"pairing": str(l.Pairing), "slack_ts": str(l.SlackTS),
		}),
	}
	// Replacing a link with a different Webex id must drop the old pointer.
	if old, err := s.get(ctx, linkPK(l.Pairing, l.SlackTS), noSort); err != nil {
		return err
	} else if old != nil && getS(old, "webex_id") != l.WebexID {
		items = append(items, s.del(webexPK(getS(old, "webex_id")), noSort))
	}
	return s.transact(ctx, items...)
}

func toLink(item map[string]types.AttributeValue) *store.Link {
	return &store.Link{
		Pairing:       getS(item, "pairing"),
		SlackTS:       getS(item, "slack_ts"),
		WebexID:       getS(item, "webex_id"),
		Origin:        model.Platform(getS(item, "origin")),
		SlackThreadTS: getS(item, "slack_thread_ts"),
		WebexParentID: getS(item, "webex_parent_id"),
	}
}

func (s *Store) LinkBySlack(ctx context.Context, pairing, ts string) (*store.Link, error) {
	item, err := s.get(ctx, linkPK(pairing, ts), noSort)
	if err != nil || item == nil {
		return nil, err
	}
	return toLink(item), nil
}

func (s *Store) LinkByWebex(ctx context.Context, webexID string) (*store.Link, error) {
	ptr, err := s.get(ctx, webexPK(webexID), noSort)
	if err != nil || ptr == nil {
		return nil, err
	}
	l, err := s.LinkBySlack(ctx, getS(ptr, "pairing"), getS(ptr, "slack_ts"))
	if err != nil || l == nil || l.WebexID != webexID {
		return nil, err
	}
	return l, nil
}

func (s *Store) DeleteLink(ctx context.Context, l store.Link) error {
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.table, Key: key(linkPK(l.Pairing, l.SlackTS), noSort), ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return err
	}
	if id := getS(out.Attributes, "webex_id"); id != "" {
		_, err = s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.table, Key: key(webexPK(id), noSort)})
	}
	return err
}

// ---------------------------------------------------------------------------
// Reaction notes

func notePK(pairing, ts, user, reaction string) string { return pk("N", pairing, ts, user, reaction) }

func (s *Store) PutReactionNote(ctx context.Context, n store.ReactionNote) error {
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &s.table,
		Item:      s.record(notePK(n.Pairing, n.SlackTS, n.User, n.Reaction), noSort, map[string]types.AttributeValue{"webex_id": str(n.WebexID)}),
	})
	return err
}

func (s *Store) TakeReactionNote(ctx context.Context, pairing, slackTS, user, reaction string) (*store.ReactionNote, error) {
	// DeleteItem with ALL_OLD is atomic: only one racing caller sees the item.
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.table, Key: key(notePK(pairing, slackTS, user, reaction), noSort), ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil || !s.live(out.Attributes) {
		return nil, err
	}
	return &store.ReactionNote{Pairing: pairing, SlackTS: slackTS, User: user, Reaction: reaction, WebexID: getS(out.Attributes, "webex_id")}, nil
}

// ---------------------------------------------------------------------------
// Webex reactions

func reactionPK(activity string) string         { return pk("R", activity) }
func membersPK(webexID, reaction string) string { return pk("RS", webexID, reaction) }

func (s *Store) countReactions(ctx context.Context, webexID, reaction string) (int, error) {
	s.mu.RLock()
	cutoff := s.purgedBefore
	s.mu.RUnlock()
	input := &dynamodb.QueryInput{
		TableName:              &s.table,
		KeyConditionExpression: aws.String("pk = :pk"),
		FilterExpression:       aws.String("(attribute_not_exists(#exp) OR #exp > :now) AND #created >= :cutoff"),
		ExpressionAttributeNames: map[string]string{
			"#exp": ttlAttr, "#created": createdAt,
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": str(membersPK(webexID, reaction)), ":now": num(s.now().Unix()), ":cutoff": num(cutoff),
		},
		Select:         types.SelectCount,
		ConsistentRead: aws.Bool(true),
	}
	total := 0
	for {
		out, err := s.db.Query(ctx, input)
		if err != nil {
			return 0, err
		}
		total += int(out.Count)
		if len(out.LastEvaluatedKey) == 0 {
			return total, nil
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

func (s *Store) AddWebexReaction(ctx context.Context, r store.WebexReaction) (int, error) {
	// A live reaction for this activity already exists: nothing to do.
	if existing, err := s.get(ctx, reactionPK(r.ActivityID), noSort); err != nil {
		return 0, err
	} else if existing != nil {
		return s.countReactions(ctx, getS(existing, "webex_id"), getS(existing, "reaction"))
	}
	err := s.transact(ctx,
		s.put(reactionPK(r.ActivityID), noSort, map[string]types.AttributeValue{
			"webex_id": str(r.WebexID), "person_id": str(r.PersonID), "reaction": str(r.Reaction),
		}),
		s.put(membersPK(r.WebexID, r.Reaction), r.ActivityID, nil),
	)
	if err != nil {
		return 0, err
	}
	return s.countReactions(ctx, r.WebexID, r.Reaction)
}

func (s *Store) RemoveWebexReaction(ctx context.Context, activityID string) (*store.WebexReaction, int, error) {
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.table, Key: key(reactionPK(activityID), noSort), ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return nil, 0, err
	}
	old := out.Attributes
	if len(old) == 0 {
		return nil, 0, nil
	}
	r := store.WebexReaction{
		ActivityID: activityID, WebexID: getS(old, "webex_id"), PersonID: getS(old, "person_id"), Reaction: getS(old, "reaction"),
	}
	if _, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.table, Key: key(membersPK(r.WebexID, r.Reaction), activityID),
	}); err != nil {
		return nil, 0, err
	}
	if !s.live(old) {
		return nil, 0, nil // it had already expired
	}
	remaining, err := s.countReactions(ctx, r.WebexID, r.Reaction)
	return &r, remaining, err
}

// ---------------------------------------------------------------------------
// Values

func (s *Store) GetValue(ctx context.Context, k string) (string, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: key(pk("KV", k), noSort), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return getS(out.Item, "v"), nil
}

func (s *Store) SetValue(ctx context.Context, k, value string) error {
	item := key(pk("KV", k), noSort)
	item["v"] = str(value)
	_, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

// Purge relies on native TTL to delete items. It records the cutoff so
// that reads hide older records right away, without scanning the table.
func (s *Store) Purge(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	if c := before.UnixMicro(); c > s.purgedBefore {
		s.purgedBefore = c
	}
	s.mu.Unlock()
	return 0, nil
}

// ---------------------------------------------------------------------------
// Event queue
//
//	pk                    sk                        item
//	EV␟{id}               "-"                       the event (never expires)
//	EQ␟{status}           {enqueued:020d}␟{id}      index entry, sorted by age
//
// Query on EQ␟pending returns events oldest first without a secondary index.

func eventPK(id string) string { return pk("EV", id) }

func queuePK(status store.EventStatus) string { return pk("EQ", string(status)) }

func queueSK(e store.QueuedEvent) string {
	return fmt.Sprintf("%020d%s%s", e.Enqueued.UnixMicro(), sep, e.ID)
}

func eventItem(e store.QueuedEvent) map[string]types.AttributeValue {
	item := key(eventPK(e.ID), noSort)
	item["id"] = str(e.ID)
	item["payload"] = &types.AttributeValueMemberB{Value: e.Payload}
	item["status"] = str(string(e.Status))
	item["enqueued"] = num(e.Enqueued.UnixMicro())
	item["attempts"] = num(int64(e.Attempts))
	item["next"] = num(e.NextAttempt.UnixMicro())
	item["last_error"] = str(e.LastError)
	return item
}

func indexItem(e store.QueuedEvent) map[string]types.AttributeValue {
	item := key(queuePK(e.Status), queueSK(e))
	item["id"] = str(e.ID)
	item["next"] = num(e.NextAttempt.UnixMicro())
	return item
}

func toEvent(item map[string]types.AttributeValue) store.QueuedEvent {
	e := store.QueuedEvent{
		ID:          getS(item, "id"),
		Status:      store.EventStatus(getS(item, "status")),
		Enqueued:    time.UnixMicro(getN(item, "enqueued")),
		Attempts:    int(getN(item, "attempts")),
		NextAttempt: time.UnixMicro(getN(item, "next")),
		LastError:   getS(item, "last_error"),
	}
	if b, ok := item["payload"].(*types.AttributeValueMemberB); ok {
		e.Payload = b.Value
	}
	return e
}

func (s *Store) EnqueueEvent(ctx context.Context, e store.QueuedEvent) error {
	_, err := s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &s.table, Item: eventItem(e), ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: &s.table, Item: indexItem(e)}},
	}})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		for _, r := range cancelled.CancellationReasons {
			if aws.ToString(r.Code) == "ConditionalCheckFailed" {
				return nil // already enqueued
			}
		}
	}
	return err
}

func (s *Store) GetEvent(ctx context.Context, id string) (*store.QueuedEvent, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: key(eventPK(id), noSort), ConsistentRead: aws.Bool(true),
	})
	if err != nil || len(out.Item) == 0 {
		return nil, err
	}
	e := toEvent(out.Item)
	return &e, nil
}

// queued walks a status index oldest first and loads matching events.
func (s *Store) queued(ctx context.Context, status store.EventStatus, limit int, filter string, values map[string]types.AttributeValue, names ...string) ([]store.QueuedEvent, error) {
	if values == nil {
		values = map[string]types.AttributeValue{}
	}
	values[":pk"] = str(queuePK(status))
	input := &dynamodb.QueryInput{
		TableName:                 &s.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: values,
		ConsistentRead:            aws.Bool(true),
	}
	if filter != "" {
		input.FilterExpression = aws.String(filter)
	}
	if len(names) > 0 {
		input.ExpressionAttributeNames = map[string]string{}
		for _, n := range names {
			input.ExpressionAttributeNames[n] = strings.TrimPrefix(n, "#")
		}
	}
	var out []store.QueuedEvent
	for len(out) < limit {
		page, err := s.db.Query(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Items {
			if len(out) == limit {
				break
			}
			e, err := s.GetEvent(ctx, getS(entry, "id"))
			if err != nil {
				return nil, err
			}
			if e != nil && e.Status == status {
				out = append(out, *e)
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			break
		}
		input.ExclusiveStartKey = page.LastEvaluatedKey
	}
	return out, nil
}

func (s *Store) DueEvents(ctx context.Context, now time.Time, limit int) ([]store.QueuedEvent, error) {
	return s.queued(ctx, store.EventPending, limit, "#next <= :now", map[string]types.AttributeValue{
		":now": num(now.UnixMicro()),
	}, "#next")
}

func (s *Store) ParkedEvents(ctx context.Context, limit int) ([]store.QueuedEvent, error) {
	return s.queued(ctx, store.EventParked, limit, "", nil)
}

func (s *Store) UpdateEvent(ctx context.Context, e store.QueuedEvent) error {
	old, err := s.GetEvent(ctx, e.ID)
	if err != nil || old == nil {
		return err
	}
	updated := *old
	updated.Status, updated.Attempts, updated.NextAttempt, updated.LastError = e.Status, e.Attempts, e.NextAttempt, e.LastError
	items := []types.TransactWriteItem{
		{Put: &types.Put{TableName: &s.table, Item: eventItem(updated)}},
		{Put: &types.Put{TableName: &s.table, Item: indexItem(updated)}},
	}
	if old.Status != updated.Status {
		items = append(items, s.del(queuePK(old.Status), queueSK(*old)))
	}
	return s.transact(ctx, items...)
}

func (s *Store) DeleteEvent(ctx context.Context, id string) error {
	old, err := s.GetEvent(ctx, id)
	if err != nil || old == nil {
		return err
	}
	return s.transact(ctx, s.del(eventPK(id), noSort), s.del(queuePK(old.Status), queueSK(*old)))
}

func (s *Store) countQueue(ctx context.Context, status store.EventStatus) (int, error) {
	input := &dynamodb.QueryInput{
		TableName:                 &s.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": str(queuePK(status))},
		Select:                    types.SelectCount,
		ConsistentRead:            aws.Bool(true),
	}
	total := 0
	for {
		out, err := s.db.Query(ctx, input)
		if err != nil {
			return 0, err
		}
		total += int(out.Count)
		if len(out.LastEvaluatedKey) == 0 {
			return total, nil
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

func (s *Store) CountEvents(ctx context.Context) (pending, parked int, err error) {
	if pending, err = s.countQueue(ctx, store.EventPending); err != nil {
		return 0, 0, err
	}
	parked, err = s.countQueue(ctx, store.EventParked)
	return pending, parked, err
}

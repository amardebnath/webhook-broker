package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/newscred/webhook-broker/storage/data"
)

// MessageDBRepository is the MessageRepository implementation
type MessageDBRepository struct {
	db                 *sql.DB
	channelRepository  ChannelRepository
	producerRepository ProducerRepository
}

var (
	// ErrDuplicateMessageIDForChannel represents when the a message with same ID already exists
	ErrDuplicateMessageIDForChannel = errors.New("duplicate message id for channel")
	// ErrNoTxInContext represents the case where transaction is not passed in the context
	ErrNoTxInContext = errors.New("no tx value in content")
	mysqlErrorMap    = map[uint16]error{
		// https://dev.mysql.com/doc/mysql-errors/8.0/en/server-error-reference.html#error_er_dup_entry
		1062: ErrDuplicateMessageIDForChannel,
	}
)

// ContextKey represents context key
type ContextKey string

const (
	messageSelectRowCommonQuery            = "SELECT id, messageId, producerId, channelId, payload, contentType, priority, status, receivedAt, outboxedAt, createdAt, updatedAt FROM message WHERE"
	txContextKey                ContextKey = "tx"
)

// Create creates a new message if message.MessageID does not already exist; please ensure QuickFix is called before repo is called
func (msgRepo *MessageDBRepository) Create(message *data.Message) (err error) {
	if !message.IsInValidState() {
		err = data.ErrInsufficientInformationForCreating
	}
	if err == nil {
		_, msgErr := msgRepo.Get(message.GetChannelIDSafely(), message.MessageID)
		if msgErr == nil {
			err = ErrDuplicateMessageIDForChannel
		} else {
			err = transactionalSingleRowWriteExec(msgRepo.db, emptyOps, "INSERT INTO message (id, channelId, producerId, messageId, payload, contentType, priority, status, receivedAt, outboxedAt, createdAt, updatedAt) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				args2SliceFnWrapper(message.ID, message.BroadcastedTo.ChannelID, message.ProducedBy.ProducerID, message.MessageID, message.Payload, message.ContentType, message.Priority, message.Status, message.ReceivedAt, message.OutboxedAt, message.CreatedAt, message.UpdatedAt))
			err = normalizeDBError(err, mysqlErrorMap)
		}
	}
	return err
}

// Get retrieves a message for a channel if it exists
func (msgRepo *MessageDBRepository) Get(channelID string, messageID string) (*data.Message, error) {
	channel, err := msgRepo.channelRepository.Get(channelID)
	if err != nil {
		return &data.Message{}, err
	}
	message, err := msgRepo.getSingleMessage(messageSelectRowCommonQuery+" channelId like ? and messageId like ?", args2SliceFnWrapper(channelID, messageID), false)
	if err == nil {
		message.BroadcastedTo = channel
	}
	return message, err
}

func (msgRepo *MessageDBRepository) getSingleMessage(query string, queryArgs func() []interface{}, loadChannel bool) (message *data.Message, err error) {
	var producerID string
	var channelID string
	message = &data.Message{}
	if err == nil {
		err = querySingleRow(msgRepo.db, query, queryArgs,
			args2SliceFnWrapper(&message.ID, &message.MessageID, &producerID, &channelID, &message.Payload, &message.ContentType, &message.Priority, &message.Status, &message.ReceivedAt, &message.OutboxedAt, &message.CreatedAt, &message.UpdatedAt))
	}
	if err == nil {
		message.ProducedBy, err = msgRepo.producerRepository.Get(producerID)
	}
	if loadChannel && err == nil {
		message.BroadcastedTo, err = msgRepo.channelRepository.Get(channelID)
	}
	return message, err
}

// GetByID retrieves a message by its ID
func (msgRepo *MessageDBRepository) GetByID(id string) (*data.Message, error) {
	return msgRepo.getSingleMessage(messageSelectRowCommonQuery+" id like ?", args2SliceFnWrapper(id), true)
}

// SetDispatched sets the status of the message to dispatched within the transaction passed via txContext
func (msgRepo *MessageDBRepository) SetDispatched(txContext context.Context, message *data.Message) error {
	if message == nil || !message.IsInValidState() {
		return ErrInvalidStateToSave
	}
	tx, ok := txContext.Value(txContextKey).(*sql.Tx)
	if ok {
		currentTime := time.Now()
		err := inTransactionExec(tx, emptyOps, "UPDATE message SET status = ?, outboxedAt = ?, updatedAt = ? WHERE id like ? and status = ?", args2SliceFnWrapper(data.MsgStatusDispatched, currentTime, currentTime, message.ID, data.MsgStatusAcknowledged), int64(1))
		if err == nil {
			message.Status = data.MsgStatusDispatched
			message.OutboxedAt = currentTime
			message.UpdatedAt = currentTime
		}
		return err
	}
	return ErrNoTxInContext
}

func (msgRepo *MessageDBRepository) getMessages(baseQuery string, args ...interface{}) ([]*data.Message, *data.Pagination, error) {
	pageMessages := make([]*data.Message, 0, 100)
	newPage := &data.Pagination{}
	scanArgs := func() []interface{} {
		msg := &data.Message{}
		msg.ProducedBy = &data.Producer{}
		msg.BroadcastedTo = &data.Channel{}
		pageMessages = append(pageMessages, msg)
		return []interface{}{&msg.ID, &msg.MessageID, &msg.ProducedBy.ProducerID, &msg.BroadcastedTo.ChannelID, &msg.Payload, &msg.ContentType, &msg.Priority, &msg.Status, &msg.ReceivedAt, &msg.OutboxedAt, &msg.CreatedAt, &msg.UpdatedAt}
	}
	err := queryRows(msgRepo.db, baseQuery, args2SliceFnWrapper(args...), scanArgs)
	if err == nil {
		for _, msg := range pageMessages {
			msg.BroadcastedTo, _ = msgRepo.channelRepository.Get(msg.BroadcastedTo.ChannelID)
			msg.ProducedBy, _ = msgRepo.producerRepository.Get(msg.ProducedBy.ProducerID)
		}
		msgCount := len(pageMessages)
		if msgCount > 0 {
			newPage = data.NewPagination(pageMessages[msgCount-1], pageMessages[0])
		}
	} else {
		log.Error().Err(err).Msg("error - could get list messages needing to be dispatched")
	}
	return pageMessages, newPage, err
}

// GetMessagesNotDispatchedForCertainPeriod retrieves messages in acknowledged state despite `delta` being passed.
func (msgRepo *MessageDBRepository) GetMessagesNotDispatchedForCertainPeriod(delta time.Duration) []*data.Message {
	messages := make([]*data.Message, 0, 100)
	if delta > 0 {
		delta = -1 * delta
	}
	earliestReceivedAt := time.Now().Add(delta)
	page := data.NewPagination(nil, nil)
	more := true
	for more {
		baseQuery := messageSelectRowCommonQuery + " status = ? AND receivedAt <= ?" + getPaginationQueryFragmentWithConfigurablePageSize(page, true, largePageSizeWithOrder)
		pageMessages, pagination, err := msgRepo.getMessages(baseQuery, appendWithPaginationArgs(page, data.MsgStatusAcknowledged, earliestReceivedAt)...)
		if err == nil {
			msgCount := len(pageMessages)
			if msgCount <= 0 {
				more = false
			} else {
				messages = append(messages, pageMessages...)
				page.Next = pagination.Next
			}
		} else {
			log.Error().Err(err).Msg("error - could get list messages needing to be dispatched")
			more = false
		}
	}
	return messages
}

// GetMessagesForChannel retrieves messages broadcasted to a specific channel
func (msgRepo *MessageDBRepository) GetMessagesForChannel(channelID string, page *data.Pagination) ([]*data.Message, *data.Pagination, error) {
	nilMessages := make([]*data.Message, 0)
	defaultEmptyPagination := &data.Pagination{}
	if page == nil || (page.Next != nil && page.Previous != nil) {
		return nilMessages, defaultEmptyPagination, ErrPaginationDeadlock
	}
	_, err := msgRepo.channelRepository.Get(channelID)
	if err != nil {
		return nilMessages, defaultEmptyPagination, err
	}
	baseQuery := messageSelectRowCommonQuery + " channelId like ?" + getPaginationQueryFragmentWithConfigurablePageSize(page, true, pageSizeWithOrder)
	return msgRepo.getMessages(baseQuery, appendWithPaginationArgs(page, channelID)...)
}

// NewMessageRepository creates a new instance of MessageRepository
func NewMessageRepository(db *sql.DB, channelRepo ChannelRepository, producerRepo ProducerRepository) MessageRepository {
	panicIfNoDBConnectionPool(db)
	return &MessageDBRepository{db: db, channelRepository: channelRepo, producerRepository: producerRepo}
}

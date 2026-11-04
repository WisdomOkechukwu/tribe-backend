package account

import (
	"context"
	"time"

	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
)

// pushBatchSize is how many inbox messages one outbox run pushes at most.
const pushBatchSize = 200

// PushPendingInboxMessages sends a push notification for every inbox message
// that has not been pushed yet. The scheduler calls it every few seconds.
//
// Why an "outbox"? Services write the inbox message INSIDE their money
// transaction. If they queued the push straight away, a rollback could leave
// a push for money that never moved, or a crash could lose the push. Reading
// committed inbox rows afterwards avoids both:
//
//  1. Find messages with pushed_at IS NULL (oldest first).
//  2. Queue a push job for each. The job id is the message id, so a message
//     queued twice is still only pushed once.
//  3. Set pushed_at, so the next run skips it.
//
// It returns how many messages were queued.
func (service *Service) PushPendingInboxMessages(requestContext context.Context) (int, error) {
	var messages []models.InboxMessage
	err := service.database.WithContext(requestContext).
		Where("pushed_at IS NULL").Order("created_at").Limit(pushBatchSize).Find(&messages).Error
	if err != nil {
		return 0, err
	}
	pushed := 0
	for _, message := range messages {
		payload := jobs.SendPushNotificationPayload{UserID: message.UserID, Title: message.Title, Body: message.Body}
		if message.Href != nil {
			payload.Href = *message.Href
		}
		if err := service.dispatcher.SendPushNotification(requestContext, message.ID, payload); err != nil {
			return pushed, err // Redis is down: try again on the next run
		}
		if err := service.database.WithContext(requestContext).Model(&models.InboxMessage{}).
			Where("id = ? AND pushed_at IS NULL", message.ID).Update("pushed_at", time.Now().UTC()).Error; err != nil {
			return pushed, err
		}
		pushed++
	}
	return pushed, nil
}

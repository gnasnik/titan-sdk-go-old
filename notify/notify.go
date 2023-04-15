package notify

import (
	"context"
	logging "github.com/ipfs/go-log"
)

var log = logging.Logger("notification")

type Notification struct {
	// endOfFileNotify is a synchronous Notification channel that signals the end of File processing.
	// This channel is used to notify when a processing operation has been completed.
	endOfFileNotify chan struct{}
	// When the handleEndOfFileEvent callback function has been successfully executed,
	// a notification is sent to this channel to indicate that processing has ended.
	done chan struct{}
}

// NewNotification returns a new Notification
func NewNotification() *Notification {
	return &Notification{
		endOfFileNotify: make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// ListenEndOfFile listens to the end of the file event, blocks until a Notification is received from the channel endOfFileNotify,
// indicating that File processing has ended and then calls the callback function handleEndOfFileEvent.
func (n *Notification) ListenEndOfFile(ctx context.Context, handleEndOfFileEvent func() error) {
	for {
		select {
		case <-n.endOfFileNotify:
			if err := handleEndOfFileEvent(); err != nil {
				log.Errorf("handle endOfFile: %v", err)
			}
			n.done <- struct{}{}
		case <-ctx.Done():
			return
		}
	}
}

// NotifyEndOfFile notifies the end of the file and signals that the file processing has been completed.
func (n *Notification) NotifyEndOfFile() {
	n.endOfFileNotify <- struct{}{}

	select {
	case <-n.done:
	}
}

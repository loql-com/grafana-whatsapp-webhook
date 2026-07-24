package whatsapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/optiop/grafana-whatsapp-webhook/pkg/entity"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	ErrWhatsAppUnavailable = errors.New("WhatsApp is not connected")
	errWhatsAppQueueFull   = errors.New("WhatsApp message queue is full")
)

type WhatsappService struct {
	client *whatsmeow.Client

	mu    sync.RWMutex
	ready bool

	cUserMessage,
	cGroupMessage chan entity.Message
}

func New() *WhatsappService {
	return &WhatsappService{
		cUserMessage:  make(chan entity.Message, 1024),
		cGroupMessage: make(chan entity.Message, 1024),
	}
}

func (ws *WhatsappService) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ws.setupWhatsappService(ctx); err != nil {
			log.Printf("WhatsApp initialization failed: %v", err)
		}
	}()
}

func (*WhatsappService) eventHandler(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		log.Println("Received a message: ", v.Message.GetConversation())
	}
}

// setupWhatsappService initializes and sets up the WhatsApp service for the given WhatsappService instance.
// It configures logging, connects to the database, retrieves the device store, and establishes a connection
// to the WhatsApp client. It also handles QR code generation for new logins and retrieves the list of joined groups.
// The function performs the following steps:
//  1. Configures logging based on the APP_DEBUG environment variable.
//  2. Connects to the SQLite database and retrieves the device store.
//  3. Initializes the WhatsApp client and sets up event handlers.
//  4. Handles QR code generation for new logins if the client is not already authenticated.
//  5. Connects the client to the WhatsApp service.
//  6. Retrieves and logs the list of joined groups.
//  7. Starts and waits for the user and group message workers.
//
// If initialization fails, the function returns the error so the HTTP service can
// remain available for its liveness probe.
func (ws *WhatsappService) setupWhatsappService(
	ctx context.Context,
) error {
	if err := os.MkdirAll("data", os.ModePerm); err != nil {
		return err
	}

	debug := strings.ToLower(os.Getenv("APP_DEBUG")) == "true"
	level := "INFO"

	if debug {
		level = "DEBUG"
	}

	dbLog := waLog.Stdout("Database", level, true)
	container, err := sqlstore.New(ctx, "sqlite3", "data/sqlite3.db?_foreign_keys=on", dbLog)
	if err != nil {
		return err
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}

	clientLog := waLog.Stdout("Client", level, true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	ws.client = client
	defer client.Disconnect()

	// connectedCh is closed on the first events.Connected, signalling that the
	// WebSocket handshake (including any 515 server-redirect reconnect) is done.
	connectedCh := make(chan struct{})
	var connectedOnce sync.Once
	client.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			connectedOnce.Do(func() { close(connectedCh) })
		}
	})
	client.AddEventHandler(ws.eventHandler)

	if client.Store.ID == nil {
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			return err
		}
		err = client.Connect()
		if err != nil {
			return err
		}
		if err := waitForQRPairing(ctx, qrChan, writeQRCode); err != nil {
			return err
		}
	} else {
		err = client.Connect()
		if err != nil {
			return err
		}
	}

	// Wait until the connection is fully established before querying groups.
	// This handles the 515 server-redirect reconnect that happens after pairing.
	select {
	case <-connectedCh:
	case <-ctx.Done():
		return nil
	}

	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		log.Printf("failed to retrieve joined WhatsApp groups: %v", err)
	} else {
		log.Println("Joined groups:")
		for _, group := range groups {
			log.Println("Name: ", group.Name)
			log.Println("Jid: ", group.JID)
			log.Println("----------------")
		}
	}

	ws.setReady(true)
	defer ws.setReady(false)

	var senderWG sync.WaitGroup
	senderWG.Add(2)
	go func() {
		defer senderWG.Done()
		ws.handleSendUserMessages(ctx)
	}()
	go func() {
		defer senderWG.Done()
		ws.handleSendGroupMessages(ctx)
	}()
	<-ctx.Done()
	senderWG.Wait()
	return nil
}

func waitForQRPairing(
	ctx context.Context,
	qrChan <-chan whatsmeow.QRChannelItem,
	onCode func(string),
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-qrChan:
			if !ok {
				return errors.New("WhatsApp QR pairing channel closed before success")
			}
			switch evt.Event {
			case "code":
				onCode(evt.Code)
			case "success":
				return nil
			case "error":
				if evt.Error != nil {
					return evt.Error
				}
				return errors.New("WhatsApp QR pairing failed")
			default:
				return fmt.Errorf("WhatsApp QR pairing failed: %s", evt.Event)
			}
		}
	}
}

func writeQRCode(code string) {
	image, err := writeQRCodeLog(code, os.Stdout)
	if err != nil {
		log.Println("Error writing QR code log entry: ", err)
		return
	}
	if err := os.MkdirAll("out", 0o755); err != nil {
		log.Println("Error creating 'out' directory: ", err)
		return
	}
	if err := os.WriteFile("out/qr.png", image, 0o600); err != nil {
		log.Println("Error: to create QR code PNG file: ", err)
	}
}

func writeQRCodeLog(code string, output io.Writer) ([]byte, error) {
	image, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("encode QR code as PNG: %w", err)
	}

	entry := struct {
		Severity    string `json:"severity"`
		Event       string `json:"event"`
		Message     string `json:"message"`
		MIMEType    string `json:"mime_type"`
		ImageBase64 string `json:"image_base64"`
	}{
		Severity:    "INFO",
		Event:       "whatsapp_qr_code",
		Message:     "WhatsApp pairing QR code generated",
		MIMEType:    "image/png",
		ImageBase64: base64.StdEncoding.EncodeToString(image),
	}
	if err := json.NewEncoder(output).Encode(entry); err != nil {
		return nil, fmt.Errorf("write structured QR code log entry: %w", err)
	}
	return image, nil
}

func (ws *WhatsappService) SendNewWhatsAppMessageToUser(msg entity.Message) error {
	return ws.enqueue(ws.cUserMessage, msg)
}

func (ws *WhatsappService) SendNewWhatsAppMessageToGroup(msg entity.Message) error {
	return ws.enqueue(ws.cGroupMessage, msg)
}

func (ws *WhatsappService) enqueue(queue chan<- entity.Message, msg entity.Message) error {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	if !ws.ready {
		return ErrWhatsAppUnavailable
	}

	select {
	case queue <- msg:
		return nil
	default:
		return errWhatsAppQueueFull
	}
}

func (ws *WhatsappService) setReady(ready bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.ready = ready
}

func (ws *WhatsappService) handleSendUserMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case msg := <-ws.cUserMessage:
			to := types.NewJID(msg.To, "s.whatsapp.net")
			message := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: &msg.Body,
				},
			}

			_, err := ws.client.SendMessage(ctx, to, message)
			if err != nil {
				log.Printf("failed to send user message to %s: %v", msg.To, err)
			}
		}
	}
}

func (ws *WhatsappService) handleSendGroupMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case msg := <-ws.cGroupMessage:
			groupJID := types.NewJID(msg.To, "g.us")
			message := &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: &msg.Body,
				},
			}

			_, err := ws.client.SendMessage(ctx, groupJID, message)
			if err != nil {
				log.Printf("failed to send group message to %s: %v", msg.To, err)
			}
		}
	}
}

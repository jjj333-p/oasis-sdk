package oasis_sdk

// upload.go implements XMPP file upload functionality according to XEP-0363 HTTP File Upload
// specification. It provides methods for requesting upload slots and performing file uploads.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/upload"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// UploadRequestDetails represents the XML structure for requesting an upload slot
// from an XMPP server. It follows the XEP-0363 specification format.
type UploadRequestDetails struct {
	XMLName     xml.Name `xml:"urn:xmpp:http:upload:0 request"`
	Filename    string   `xml:"filename,attr"`     // Name of file to be uploaded
	Size        int64    `xml:"size,attr"`         // Size of file in bytes
	ContentType *string  `xml:"content-type,attr"` // Optional MIME type of the file
}

type Header struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

type PutURL struct {
	URL     string   `xml:"url,attr"`
	Headers []Header `xml:"header"`
}

type GetURL struct {
	URL string `xml:"url,attr"`
}

type UploadSlotResponsePayload struct {
	XMLName xml.Name `xml:"urn:xmpp:http:upload:0 slot"`
	Put     PutURL   `xml:"put"`
	Get     GetURL   `xml:"get"`
}

type UploadSlotResponse struct {
	stanza.IQ
	Slot UploadSlotResponsePayload `xml:"slot"`
}

// getUploadSlot requests an upload slot from the XMPP server's HTTP upload component.
// It returns the PUT URL with headers for uploading and the GET URL for retrieving the file.
// Returns an error if the upload component isn't available or if the file size exceeds limits.
// Times out after 30 seconds if a slot is not acquired.
func (client *XmppClient) getUploadSlot(parentCtx context.Context, request upload.File) (*upload.Slot, error) {
	if client.HttpUploadComponent == nil || client.HttpUploadComponent.Jid.String() == "" {
		return nil, errors.New("no upload component found yet, try discovering services")
	}

	//we assume server is telling the truth
	if request.Size > client.HttpUploadComponent.MaxFileSize {
		return nil, fmt.Errorf(
			"upload size too large, want %d, have %d",
			request.Size, client.HttpUploadComponent.MaxFileSize,
		)
	}

	slotCtx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel() // Important to prevent context leak

	// use mellium function to get slot
	slot, err := upload.GetSlot(slotCtx, request, client.HttpUploadComponent.Jid, client.Session)

	//convert return values
	if err != nil {
		return nil, err
	}
	return &slot, nil
}

// UploadProgress represents the current status of an upload operation
type UploadProgress struct {
	BytesSent  int
	TotalBytes int
	Percentage float32
	GetURL     string // Only set when upload is complete
	Error      error  // Set if an error occurs
}

// progressReader wraps an io.Reader to track upload progress
type progressReader struct {
	reader       io.Reader
	bytesRead    int
	totalSize    int
	progressFunc func(int)
}

// Read reads data into the provided byte slice and updates the progress tracking information.
func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.bytesRead += n
	if pr.progressFunc != nil {
		pr.progressFunc(pr.bytesRead)
	}
	return n, err
}

// sendProgress sends the current upload progress, including bytes sent, total bytes, percentage, any error, and getURL.
// It writes the update to progressChan without blocking if the channel is not ready.
// Parameters: bytesSent is the number of bytes uploaded, totalBytes is the total size of the upload, err is any error
// encountered, getURL is the download URL if upload completes successfully, and progressChan is the channel for progress.
func sendProgress(bytesSent int, totalBytes int, err error, getURL string, progressChan chan<- UploadProgress) {
	if progressChan == nil {
		return
	}
	progress := UploadProgress{
		BytesSent:  bytesSent,
		TotalBytes: totalBytes,
		Percentage: float32(bytesSent) / float32(totalBytes) * 100,
		Error:      err,
		GetURL:     getURL,
	}
	select {
	case progressChan <- progress:
	default:
		// Don't block if receiver is not ready
	}
}

// UploadFileFromBytes handles the complete process of uploading a file to the XMPP server.
// It first requests an upload slot, then performs the HTTP PUT request to upload the file.
// This method should be executed in a goroutine. Upload progress and status updates are sent through
// the progressChan channel, which will be closed when the upload completes or fails.
// Returns the GET URL where the file can be downloaded from, or an error if the upload fails.
func (client *XmppClient) UploadFileFromBytes(
	ctx context.Context,
	filename string,
	content []byte,
	progressChan chan<- UploadProgress,
) {
	if progressChan != nil {
		defer close(progressChan)
	}

	if filename == "" || len(content) == 0 {
		sendProgress(0, 0, errors.New("filename and content cannot be empty"), "", progressChan)
		return
	}

	// put together data
	request := upload.File{
		Name: filepath.Base(filename),
		Size: len(content),
	}

	// request upload slot
	slot, err := client.getUploadSlot(ctx, request)
	if err != nil {
		sendProgress(0, request.Size, fmt.Errorf("failed to get upload slot: %w", err), "", progressChan)
		return
	}

	//sanity check
	if slot == nil || slot.PutURL == nil || slot.GetURL == nil {
		sendProgress(0, request.Size, errors.New("upload slot response from the server is malformed"), "", progressChan)
		return
	}

	// Create a custom reader that reports progress
	reader := &progressReader{
		reader:       bytes.NewReader(content),
		totalSize:    request.Size,
		progressFunc: func(n int) { sendProgress(n, request.Size, nil, "", progressChan) },
	}

	//create new request object
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.PutURL.String(), reader)
	if err != nil {
		sendProgress(0, request.Size, fmt.Errorf("failed to create upload request: %w", err), "", progressChan)
		return
	}

	//Copy headers from slot.Header
	for k, v := range slot.Header {
		req.Header[k] = v
	}

	//make request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		sendProgress(reader.bytesRead, request.Size, fmt.Errorf("failed to upload file: %w", err), "", progressChan)
		return
	}
	defer resp.Body.Close()

	//check if request succeeded
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		sendProgress(reader.bytesRead, request.Size,
			fmt.Errorf("upload failed with status code: %d", resp.StatusCode), "", progressChan)
		return
	}

	// Send final progress with GetURL
	sendProgress(request.Size, request.Size, nil, slot.GetURL.String(), progressChan)
}


// UploadFile handles the complete process of uploading a file to the XMPP server.
// It first requests an upload slot, then performs the HTTP PUT request to upload the file.
// This method should be executed in a goroutine. Upload progress and status updates are sent through
// the progressChan channel, which will be closed when the upload completes or fails.
// Returns the GET URL where the file can be downloaded from, or an error if the upload fails.
func (client *XmppClient) UploadFile(
	ctx context.Context,
	path string,
	progressChan chan<- UploadProgress,
) {
	if progressChan != nil {
		defer close(progressChan)
	}

	if path == "" {
		sendProgress(0, 0, errors.New("path cannot be empty"), "", progressChan)
		return
	}

	//open file
	file, err := os.Open(path)
	if err != nil {
		sendProgress(0, 0, fmt.Errorf("failed to open file: %w", err), "", progressChan)
		return
	}
	defer file.Close()

	//get metadata
	fileInfo, err := file.Stat()
	if err != nil {
		sendProgress(0, 0, fmt.Errorf("failed to get file info: %w", err), "", progressChan)
		return
	}

	// put together data
	request := upload.File{
		Name: filepath.Base(path),
		Size: int(fileInfo.Size()),
	}

	// request upload slot
	slot, err := client.getUploadSlot(ctx, request)
	if err != nil {
		sendProgress(0, request.Size, fmt.Errorf("failed to get upload slot: %w", err), "", progressChan)
		return
	}

	//sanity check
	if slot == nil || slot.PutURL == nil || slot.GetURL == nil {
		sendProgress(0, request.Size, errors.New("upload slot response from the server is malformed"), "", progressChan)
		return
	}

	// Create a progress tracking reader
	reader := &progressReader{
		reader:       file,
		totalSize:    int(fileInfo.Size()),
		progressFunc: func(n int) { sendProgress(n, int(fileInfo.Size()), nil, "", progressChan) },
	}

	//create new request object with context for cancellation
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.PutURL.String(), reader)
	if err != nil {
		sendProgress(0, request.Size, fmt.Errorf("failed to create upload request: %w", err), "", progressChan)
		return
	}

	// explicitly set the Content-Length header
	req.ContentLength = fileInfo.Size()

	//transfer headers
	for k, v := range slot.Header {
		req.Header[k] = v
	}

	//make request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		sendProgress(reader.bytesRead, request.Size, fmt.Errorf("failed to upload file: %w", err), "", progressChan)
		return
	}
	defer resp.Body.Close()

	//check if request succeeded
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		sendProgress(reader.bytesRead, request.Size,
			fmt.Errorf("upload failed with status code: %d", resp.StatusCode), "", progressChan)
		return
	}

	// Send final progress with GetURL
	sendProgress(request.Size, request.Size, nil, slot.GetURL.String(), progressChan)
}

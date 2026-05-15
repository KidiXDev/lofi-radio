package bootstrap

import "time"

type ProgressEventType string

const (
	ProgressEventStatus   ProgressEventType = "status"
	ProgressEventDownload ProgressEventType = "download"
)

type DownloadProgress struct {
	BytesReceived int64
	TotalBytes    int64
	SpeedPerSec   float64
	ETA           time.Duration
	Done          bool
}

type ProgressEvent struct {
	Type      ProgressEventType
	Component string
	Message   string
	Download  DownloadProgress
}

type ProgressReporter interface {
	ReportProgress(event ProgressEvent)
}

type ProgressReporterFunc func(event ProgressEvent)

func (f ProgressReporterFunc) ReportProgress(event ProgressEvent) {
	if f != nil {
		f(event)
	}
}

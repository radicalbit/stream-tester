package recordtester

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/stream-tester/apis/livepeer"
	"github.com/livepeer/stream-tester/internal/testers"
	"github.com/livepeer/stream-tester/messenger"
	"github.com/livepeer/stream-tester/model"
)

type (
	// IRecordTester ...
	IRecordTester interface {
		// Start start test. Blocks until finished.
		Start(fileName string, testDuration, pauseDuration time.Duration) (int, error)
		Cancel()
		Done() <-chan struct{}
		VODStats() model.VODStats
		Clean()
		StreamID() string
		Stream() *livepeer.CreateStreamResp
	}

	recordTester struct {
		lapi        *livepeer.API
		useForceURL bool
		ctx         context.Context
		cancel      context.CancelFunc
		vodeStats   model.VODStats
		streamID    string
		stream      *livepeer.CreateStreamResp
	}
)

var standardProfiles = []livepeer.Profile{
	{
		Name:    "240p0",
		Fps:     0,
		Bitrate: 250000,
		Width:   426,
		Height:  240,
		Gop:     "2.0",
	},
	{
		Name:    "360p0",
		Fps:     0,
		Bitrate: 800000,
		Width:   640,
		Height:  360,
		Gop:     "2.0",
	},
	{
		Name:    "480p0",
		Fps:     0,
		Bitrate: 1600000,
		Width:   854,
		Height:  480,
		Gop:     "2.0",
	},
	{
		Name:    "720p0",
		Fps:     0,
		Bitrate: 3000000,
		Width:   1280,
		Height:  720,
		Gop:     "2.0",
	},
}

// NewRecordTester ...
func NewRecordTester(gctx context.Context, lapi *livepeer.API, useForceURL bool) IRecordTester {
	ctx, cancel := context.WithCancel(gctx)
	rt := &recordTester{
		lapi:        lapi,
		useForceURL: useForceURL,
		ctx:         ctx,
		cancel:      cancel,
	}
	return rt
}

func (rt *recordTester) Start(fileName string, testDuration, pauseDuration time.Duration) (int, error) {
	defer rt.cancel()
	var err error
	var ingests []livepeer.Ingest
	apiTry := 0
	for {
		ingests, err = rt.lapi.Ingest(false)
		if err != nil {
			if testers.Timedout(err) && apiTry < 3 {
				apiTry++
				continue
			}
			// exit(255, fileName, *fileArg, err)
			return 255, err
		}
		break
	}
	apiTry = 0
	glog.Infof("Got ingests: %+v", ingests)
	var broadcasters []string
	for {
		broadcasters, err = rt.lapi.Broadcasters()
		if err != nil {
			if testers.Timedout(err) && apiTry < 3 {
				apiTry++
				continue
			}
			// exit(255, fileName, *fileArg, err)
			return 255, err
		}
		break
	}
	apiTry = 0
	glog.Infof("Got broadcasters: %+v", broadcasters)
	fmt.Printf("Streaming video file '%s'\n", fileName)
	/*
		httpIngestURLTemplates := make([]string, 0, len(broadcasters))
		for _, b := range broadcasters {
			httpIngestURLTemplates = append(httpIngestURLTemplates, fmt.Sprintf("%s/live/%%s", b))
		}
	*/
	httpIngest := false
	if httpIngest && len(broadcasters) == 0 {
		// exit(254, fileName, *fileArg, errors.New("Empty list of broadcasters"))
		return 254, errors.New("empty list of broadcasters")
	} else if !httpIngest && len(ingests) == 0 {
		return 254, errors.New("empty list of ingests")
		// exit(254, fileName, *fileArg, errors.New("Empty list of ingests"))
	}
	// glog.Infof("All cool!")
	hostName, _ := os.Hostname()
	streamName := fmt.Sprintf("%s_%s", hostName, time.Now().Format("2006-01-02T15:04:05Z07:00"))
	var stream *livepeer.CreateStreamResp
	for {
		stream, err = rt.lapi.CreateStreamEx(streamName, true, nil, standardProfiles...)
		if err != nil {
			if testers.Timedout(err) && apiTry < 3 {
				apiTry++
				continue
			}
			glog.Errorf("Error creating stream using Livepeer API: %v", err)
			// exit(253, fileName, *fileArg, err)
			return 253, err
		}
		break
	}
	apiTry = 0
	rt.streamID = stream.ID
	rt.stream = stream
	messenger.SendMessage(fmt.Sprintf(":information_source: Created stream id=%s", stream.ID))
	// createdAPIStreams = append(createdAPIStreams, stream.ID)
	glog.V(model.VERBOSE).Infof("Created Livepeer stream id=%s streamKey=%s playbackId=%s name=%s", stream.ID, stream.StreamKey, stream.PlaybackID, streamName)
	// glog.Infof("Waiting 5 second for stream info to propagate to the Postgres replica")
	// time.Sleep(5 * time.Second)
	rtmpURL := fmt.Sprintf("%s/%s", ingests[0].Ingest, stream.StreamKey)
	// rtmpURL = fmt.Sprintf("%s/%s", ingests[0].Ingest, stream.ID)

	mediaURL := fmt.Sprintf("%s/%s/index.m3u8", ingests[0].Playback, stream.PlaybackID)
	glog.V(model.SHORT).Infof("RTMP: %s", rtmpURL)
	glog.V(model.SHORT).Infof("MEDIA: %s", mediaURL)

	sr2 := testers.NewStreamer2(rt.ctx, false, false, false, false, false)
	go sr2.StartStreaming(fileName, rtmpURL, mediaURL, 30*time.Second, testDuration)
	<-sr2.Done()
	srerr := sr2.Err()
	glog.Infof("Streaming stream id=%s done err=%v", stream.ID, srerr)
	var re *testers.RTMPError
	if errors.As(srerr, &re) {
		return 2, re
	}
	if srerr != nil {
		glog.Warning("Streaming returned error err=%v", srerr)
		return 3, err
	}
	stats, err := sr2.Stats()
	if err != nil {
		glog.Warning("Stats returned error err=%v", err)
		return 21, err
	}
	glog.Infof("Streaming success rate=%v", stats.SuccessRate)
	if err = rt.isCancelled(); err != nil {
		return 0, err
	}
	if pauseDuration > 0 {
		glog.Infof("Pause specified, waiting %s before streaming second time", pauseDuration)
		time.Sleep(pauseDuration)
		sr2 := testers.NewStreamer2(rt.ctx, false, false, false, false, false)
		go sr2.StartStreaming(fileName, rtmpURL, mediaURL, 30*time.Second, testDuration)
		<-sr2.Done()
		srerr := sr2.Err()
		glog.Infof("Streaming second stream id=%s done", stream.ID)
		var re *testers.RTMPError
		if errors.As(srerr, &re) {
			return 2, re
		}
		if srerr != nil {
			glog.Warning("Streaming second returned error err=%v", srerr)
			return 3, err
		}
		stats, err := sr2.Stats()
		if err != nil {
			glog.Warning("Stats returned error err=%v", err)
			return 21, err
		}
		glog.Infof("Streaming second time success rate=%v", stats.SuccessRate)
		if err = rt.isCancelled(); err != nil {
			return 0, err
		}
		testDuration *= 2
	}
	glog.Infof("Waiting 10 seconds")
	time.Sleep(10 * time.Second)
	// now get sessions
	sessions, err := rt.lapi.GetSessionsR(stream.ID, false)
	if err != nil {
		glog.Errorf("Error getting sessions for stream id=%s err=%v", stream.ID, err)
		// exit(252, fileName, *fileArg, err)
		return 252, err
	}
	glog.Infof("Sessions: %+v", sessions)
	if len(sessions) != 1 {
		err := fmt.Errorf("Should have one session, got %d", len(sessions))
		glog.Error(err)
		// exit(251, fileName, *fileArg, err)
		return 251, err
	}
	sess := sessions[0]
	if len(sess.Profiles) != len(stream.Profiles) {
		err := fmt.Errorf("Got %d, but should have %d", len(sess.Profiles), len(stream.Profiles))
		return 251, err
		// exit(251, fileName, *fileArg, err)
	}
	if sess.RecordingStatus != livepeer.RecordingStatusWaiting {
		err := fmt.Errorf("Recording status is %s but should be %s", sess.RecordingStatus, livepeer.RecordingStatusWaiting)
		return 250, err
		// exit(250, fileName, *fileArg, err)
	}
	if err = rt.isCancelled(); err != nil {
		return 0, err
	}

	glog.Info("Streaming done, waiting for recording URL to appear")
	if rt.useForceURL {
		time.Sleep(5 * time.Second)
	} else {
		time.Sleep(6*time.Minute + 20*time.Second)
	}
	if err = rt.isCancelled(); err != nil {
		return 0, err
	}

	sessions, err = rt.lapi.GetSessionsR(stream.ID, rt.useForceURL)
	if err != nil {
		err := fmt.Errorf("error getting sessions for stream id=%s err=%v", stream.ID, err)
		return 252, err
		// exit(252, fileName, *fileArg, err)
	}
	glog.Infof("Sessions: %+v", sessions)
	if err = rt.isCancelled(); err != nil {
		return 0, err
	}

	sess = sessions[0]
	statusShould := livepeer.RecordingStatusReady
	if rt.useForceURL {
		statusShould = livepeer.RecordingStatusWaiting
	}
	if sess.RecordingStatus != statusShould {
		err := fmt.Errorf("Recording status is %s but should be %s", sess.RecordingStatus, statusShould)
		return 240, err
		// exit(250, fileName, *fileArg, err)
	}
	if sess.RecordingURL == "" {
		err := fmt.Errorf("Recording URL should appear by now")
		return 249, err
		// exit(249, fileName, *fileArg, err)
	}
	glog.Infof("recordingURL=%s downloading now", sess.RecordingURL)

	// started := time.Now()
	// downloader := testers.NewM3utester2(gctx, sess.RecordingURL, false, false, false, false, 5*time.Second, nil)
	// <-downloader.Done()
	// glog.Infof(`Pulling stopped after %s`, time.Since(started))
	// exit(55, fileName, *fileArg, err)
	glog.Info("Done")
	// lapi.DeleteStream(stream.ID)
	// exit(0, fileName, *fileArg, err)
	if err = rt.isCancelled(); err != nil {
		return 0, err
	}
	es, err := rt.checkDown(stream, sess.RecordingURL, testDuration, pauseDuration > 0)
	if es == 0 {
		rt.lapi.DeleteStream(stream.ID)
		// exit(0, fileName, *fileArg, err)
	}

	// uploader := testers.NewRtmpStreamer(gctx, rtmpURL)
	// uploader.StartUpload(fileName, rtmpURL, -1, 30*time.Second)
	return es, err
}

func (rt *recordTester) isCancelled() error {
	select {
	case <-rt.ctx.Done():
		return context.Canceled
	default:
	}
	return nil
}

func (rt *recordTester) checkDown(stream *livepeer.CreateStreamResp, url string, streamDuration time.Duration, doubled bool) (int, error) {
	es := 0
	started := time.Now()
	downloader := testers.NewM3utester2(rt.ctx, url, false, false, false, false, 5*time.Second, nil, false)
	<-downloader.Done()
	glog.Infof(`Pulling for %s (%s) stopped after %s`, stream.ID, stream.PlaybackID, time.Since(started))
	if err := rt.isCancelled(); err != nil {
		return 0, err
	}
	vs := downloader.VODStats()
	rt.vodeStats = vs
	if len(vs.SegmentsNum) != len(standardProfiles)+1 {
		glog.Warningf("Number of renditions doesn't match! Has %d should %d", len(vs.SegmentsNum), len(standardProfiles)+1)
		es = 35
	}
	glog.Infof("Stats for %s: %s", stream.ID, vs.String())
	glog.Infof("Stats for %s raw: %+v", stream.ID, vs)
	if ok, ers := vs.IsOk(streamDuration, doubled); !ok {
		glog.Warningf("NOT OK! (%s)", ers)
		es = 36
		return es, errors.New(ers)
	} else {
		glog.Infoln("All ok!")
	}
	return es, nil
}

func (rt *recordTester) Cancel() {
	rt.cancel()
}

func (rt *recordTester) Done() <-chan struct{} {
	return rt.ctx.Done()
}

func (rt *recordTester) VODStats() model.VODStats {
	return rt.vodeStats
}

func (rt *recordTester) Clean() {
	if rt.streamID != "" {
		rt.lapi.DeleteStream(rt.streamID)
	}
}

func (rt *recordTester) StreamID() string {
	return rt.streamID
}

func (rt *recordTester) Stream() *livepeer.CreateStreamResp {
	return rt.stream
}

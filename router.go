package wx

import (
	"errors"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
	"io"
	"math/rand"
	"sync"
	"time"
)

var (
	ErrUnpublished  = errors.New("unpublished")
	ErrEmptySession = errors.New("remote session must not be nil")
)

type Router struct {
	id string
	mu *sync.Mutex

	wtAPI    *webrtc.API
	wtConfig webrtc.Configuration

	publisher   *webrtc.PeerConnection
	subscribers map[string]*webrtc.PeerConnection

	videoTrack *webrtc.Track
	audioTrack *webrtc.Track

	trackInfos map[uint32]*trackInfo
}

func NewRouter(id string, api *webrtc.API, config webrtc.Configuration) (*Router, error) {
	var r = &Router{}
	r.id = id
	r.mu = &sync.Mutex{}
	r.wtAPI = api
	r.wtConfig = config
	r.subscribers = make(map[string]*webrtc.PeerConnection)
	r.trackInfos = make(map[uint32]*trackInfo)
	return r, nil
}

func (this *Router) GetId() string {
	return this.id
}

func (this *Router) initPub(remoteSession *webrtc.SessionDescription) (localSession *webrtc.SessionDescription, err error) {
	var peer *webrtc.PeerConnection

	logger.Printf("发布者 [%s] 开始初始化... \n", this.id)

	if peer, err = this.wtAPI.NewPeerConnection(this.wtConfig); err != nil {
		logger.Printf("发布者 [%s] 初始化 PeerConnection 发生错误 [%s] \n", this.id, err)
		return nil, err
	}

	if _, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if _, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Printf("发布者 [%s] 当前状态变更为 [%v] \n", this.id, state)
		if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			if peer == nil {
				return
			}
			peer.Close()
			peer = nil
			logger.Printf("发布者 [%s] 取消发布 \n", this.id)
		}
	})

	if this.audioTrack == nil {
		if this.audioTrack, err = peer.NewTrack(webrtc.DefaultPayloadTypeOpus, rand.Uint32(), webrtc.RTPCodecTypeAudio.String(), this.id); err != nil {
			logger.Printf("发布者 [%s] 创建 Audio track 发生错误 [%s] \n", this.id, err)
			return
		}
		logger.Printf("发布者 [%s] 创建 Audio track 成功 \n", this.id)
	}

	if this.videoTrack == nil {
		if this.videoTrack, err = peer.NewTrack(webrtc.DefaultPayloadTypeVP8, rand.Uint32(), webrtc.RTPCodecTypeVideo.String(), this.id); err != nil {
			logger.Printf("发布者 [%s] 创建 Video track 发生错误 [%s] \n", this.id, err)
			return
		}
		logger.Printf("发布者 [%s] 创建 Video track 成功 \n", this.id)
	}

	peer.OnTrack(func(track *webrtc.Track, receiver *webrtc.RTPReceiver) {
		switch track.Kind() {
		case webrtc.RTPCodecTypeVideo:
			go func() {
				peer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}})

				ticker := time.NewTicker(time.Second * 3)
				for range ticker.C {
					if peer != nil {
						if err := peer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: track.SSRC()}}); err != nil {
							return
						}
					}
				}
			}()
			this.rewriteRTP(track, this.videoTrack)
		case webrtc.RTPCodecTypeAudio:
			this.rewriteRTP(track, this.audioTrack)
		}
	})

	if err = peer.SetRemoteDescription(*remoteSession); err != nil {
		return nil, err
	}

	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	if err = peer.SetLocalDescription(answer); err != nil {
		return nil, err
	}

	this.publisher = peer

	return this.publisher.LocalDescription(), nil
}

func (this *Router) Publish(remoteSession *webrtc.SessionDescription) (localSession *webrtc.SessionDescription, err error) {
	if remoteSession == nil {
		return nil, ErrEmptySession
	}

	this.mu.Lock()
	defer this.mu.Unlock()

	if this.publisher != nil {
		this.publisher.Close()
	}

	return this.initPub(remoteSession)
}

func (this *Router) addSub(subscriber string, remoteSession *webrtc.SessionDescription) (localSession *webrtc.SessionDescription, err error) {
	var peer *webrtc.PeerConnection

	logger.Printf("订阅者 [%s - %s] 开始初始化... \n", this.id, subscriber)

	if peer, err = this.wtAPI.NewPeerConnection(this.wtConfig); err != nil {
		logger.Printf("订阅者 [%s - %s] 初始化 PeerConnection 发生错误 [%s] \n", this.id, subscriber, err)
		return nil, err
	}
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Printf("订阅者 [%s - %s] 当前状态变更为 [%v] \n", this.id, subscriber, state)
		if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			this.mu.Lock()
			defer this.mu.Unlock()
			var sub, ok = this.subscribers[subscriber]
			if ok && sub == peer {
				delete(this.subscribers, subscriber)
				peer.Close()
				logger.Printf("订阅者 [%s - %s] 取消订阅 \n", this.id, subscriber)
			}
			peer = nil
		} else if state == webrtc.PeerConnectionStateConnected {
			this.mu.Lock()
			defer this.mu.Unlock()
			this.subscribers[subscriber] = peer
		}
	})

	peer.AddTrack(this.videoTrack)
	peer.AddTrack(this.audioTrack)

	if err = peer.SetRemoteDescription(*remoteSession); err != nil {
		return nil, err
	}

	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	if err = peer.SetLocalDescription(answer); err != nil {
		return
	}

	return peer.LocalDescription(), nil
}

func (this *Router) Subscribe(subscriber string, remoteSession *webrtc.SessionDescription) (localSession *webrtc.SessionDescription, err error) {
	if remoteSession == nil {
		return nil, ErrEmptySession
	}

	this.mu.Lock()
	if this.videoTrack == nil || this.audioTrack == nil {
		this.mu.Unlock()
		return nil, ErrUnpublished
	}
	var sub = this.subscribers[subscriber]
	this.mu.Unlock()

	if sub != nil {
		sub.Close()
	}

	localSession, err = this.addSub(subscriber, remoteSession)
	if err != nil {
		return nil, err
	}

	return localSession, nil
}

func (this *Router) Unsubscribe(subscriber string) {
	this.mu.Lock()
	var sub = this.subscribers[subscriber]
	delete(this.subscribers, subscriber)
	defer this.mu.Unlock()

	if sub != nil {
		sub.Close()
	}
}

type trackInfo struct {
	timestamp      uint32
	sequenceNumber uint16
}

func (this *Router) rewriteRTP(src, dst *webrtc.Track) error {
	var err error
	defer func() {
		logger.Printf("发布者 [%s] 停止转发 [%v] 数据 \n", this.id, src.Kind())

	}()
	logger.Printf("发布者 [%s] 开始转发 [%v] 数据 \n", this.id, src.Kind())

	var info = this.trackInfos[dst.SSRC()]
	if info == nil {
		info = &trackInfo{}
		this.trackInfos[dst.SSRC()] = info
	}

	var lastTimestamp uint32 // 用于记录当前 track 最后一包的 timestamp 信息
	var tempTimestamp uint32 // 中间变量
	var packet *rtp.Packet

	for ; ; info.sequenceNumber++ {
		packet, err = src.ReadRTP()
		if err != nil {
			return err
		}

		// 记录当前包的 timestamp  信息
		tempTimestamp = packet.Timestamp
		if lastTimestamp == 0 {
			// 如果 lastTimestamp 为 0，是第一个数据包，则把该包的 timestamp 设置为 0
			packet.Timestamp = 0
		} else {
			// 如果 lastTimestamp 不为 0，不是第一个数据包，则把该包的 timestamp 设置为距离上一包的时间差
			packet.Timestamp -= lastTimestamp
		}
		// 将 lastTimestamp 设置为当前包的 timestamp
		lastTimestamp = tempTimestamp

		// 修正并记录下该 track 的正常 timestamp 信息
		info.timestamp += packet.Timestamp

		packet.Timestamp = info.timestamp
		packet.SequenceNumber = info.sequenceNumber
		packet.SSRC = dst.SSRC()
		err = dst.WriteRTP(packet)
		if err != nil && err != io.ErrClosedPipe {
			return err
		}
	}
	return nil
}

func (this *Router) LocalDescription() *webrtc.SessionDescription {
	this.mu.Lock()
	defer this.mu.Unlock()
	if this.publisher != nil {
		return this.publisher.LocalDescription()
	}
	return nil
}

func (this *Router) Close() error {
	this.mu.Lock()
	defer this.mu.Unlock()

	logger.Printf("发布者 [%s] 关闭 \n", this.id)

	if this.publisher != nil {
		this.publisher.Close()
	}
	for key, sub := range this.subscribers {
		sub.Close()
		delete(this.subscribers, key)
		logger.Printf("发布者 [%s] 清除订阅者 [%s] \n", this.id, key)
	}
	return nil
}

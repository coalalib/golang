package coalago

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
)

var (
	ErrUnsupportedType = errors.New("unsupported type of message")
	globalSessions     = newSessionStorage()
	handlersStateCache = cache.New(sumTimeAttempts, time.Second)
	proxyIDSessions    sync.Map
)

type transport struct {
	conn           dialer
	block2channels sync.Map
	block1channels sync.Map
	privateKey     []byte
}

func newtransport(conn dialer) *transport {
	sr := new(transport)
	sr.conn = conn

	return sr
}

func (tr *transport) SetPrivateKey(pk []byte) {
	tr.privateKey = pk
}

func (sr *transport) Send(message *CoAPMessage) (resp *CoAPMessage, err error) {
	switch message.Type {
	case CON:

		if message.GetScheme() == COAPS_SCHEME {
			proxyAddr := message.ProxyAddr
			if len(proxyAddr) > 0 {
				proxyID := setProxyIDIfNeed(message)
				proxyAddr = fmt.Sprintf("%v%v", proxyAddr, proxyID)
			}

			_, err := handshake(sr, message, sr.conn.RemoteAddr(), proxyAddr)
			if err != nil {
				return nil, err
			}
		}

		resp, err := sr.sendCON(message)
		if err == ErrorSessionExpired || err == ErrorSessionNotFound {
			if message.GetScheme() == COAPS_SCHEME {
				proxyAddr := message.ProxyAddr
				if len(proxyAddr) > 0 {
					proxyID := setProxyIDIfNeed(message)
					proxyAddr = fmt.Sprintf("%v%v", proxyAddr, proxyID)
				}
				_, err := handshake(sr, message, sr.conn.RemoteAddr(), proxyAddr)
				if err != nil {
					return nil, err
				}
			}

			resp, err = sr.sendCON(message)
		}
		return resp, err
	case RST, NON:
		return nil, sr.sendToSocket(message)
	default:
		return nil, ErrUnsupportedType
	}
}

func (sr *transport) SendTo(message *CoAPMessage, addr net.Addr) (resp *CoAPMessage, err error) {
	switch message.Type {
	case ACK, NON, RST:
		return nil, sr.sendACKTo(message, addr)
	default:
		return nil, ErrUnsupportedType
	}
}

func (sr *transport) sendCON(message *CoAPMessage) (resp *CoAPMessage, err error) {
	if isBigPayload(message) {
		resp, err = sr.sendARQBlock1CON(message)
		return
	}

	data, err := preparationSendingMessage(sr, message, sr.conn.RemoteAddr())
	if err != nil {
		return nil, err
	}

	attempts := 0

	for {
		if attempts > 0 {
			MetricRetransmitMessages.Inc()
		}
		attempts++
		MetricSentMessages.Inc()
		_, err = sr.conn.Write(data)
		if err != nil {
			MetricSentMessageErrors.Inc()
			return nil, err
		}

		resp, err = receiveMessage(sr, message)
		if err == ErrMaxAttempts {
			if attempts == maxSendAttempts {
				MetricExpiredMessages.Inc()
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}

		if isPingACK(resp) {
			return resp, nil
		}

		if resp.Type == ACK && resp.Code == CoapCodeEmpty {
			resp, err = sr.receiveARQBlock2(message, nil)
			return resp, err
		}

		if resp.GetBlock2() != nil {
			resp, err = sr.receiveARQBlock2(message, resp)
			return resp, err
		}

		break
	}

	return
}

func isPingACK(resp *CoAPMessage) bool {
	return resp.Type == RST && resp.Code == CoapCodeEmpty
}

func (sr *transport) sendACKTo(message *CoAPMessage, addr net.Addr) (err error) {
	if message.Type == ACK {
		if isBigPayload(message) {
			ch := make(chan *CoAPMessage, 102400)
			id := addr.String() + message.GetTokenString()
			sr.block2channels.Store(id, ch)
			err = sr.sendARQBlock2ACK(ch, message, addr)
			sr.block2channels.Delete(id)
			return err
		}
	}

	return sr.sendToSocketByAddress(message, addr)
}

func (sr *transport) sendToSocket(message *CoAPMessage) error {
	buf, err := preparationSendingMessage(sr, message, sr.conn.RemoteAddr())
	if err != nil {
		return err
	}
	MetricSentMessages.Inc()
	_, err = sr.conn.Write(buf)
	if err != nil {
		MetricSentMessageErrors.Inc()
	}
	buf = nil
	return err
}

func (sr *transport) sendToSocketByAddress(message *CoAPMessage, addr net.Addr) error {

	buf, err := preparationSendingMessage(sr, message, addr)
	if err != nil {
		return err
	}
	MetricSentMessages.Inc()
	_, err = sr.conn.WriteTo(buf, addr.String())
	if err != nil {
		MetricSentMessageErrors.Inc()
	}
	buf = nil
	return err
}

func (sr *transport) sendPackets(packets []*packet, windowsize int, shift int) error {
	stop := shift + windowsize
	if stop >= len(packets) {
		stop = len(packets)
	}

	var acked int
	for i := 0; i < stop; i++ {
		if !packets[i].acked {
			if time.Since(packets[i].lastSend) >= timeWait {
				if packets[i].attempts > 0 {
					MetricRetransmitMessages.Inc()
				}
				if packets[i].attempts == maxSendAttempts {
					MetricExpiredMessages.Inc()
					return ErrMaxAttempts
				}
				packets[i].attempts++
				packets[i].lastSend = time.Now()
				if err := sr.sendToSocket(packets[i].message); err != nil {
					return err
				}
			}
		} else {
			acked++
		}
	}

	if len(packets) == stop {
		if time.Since(packets[len(packets)-1].lastSend) >= timeWait {
			MetricExpiredMessages.Inc()
			return ErrMaxAttempts
		}
	}

	return nil
}

func (sr *transport) sendPacketsByWindowOffset(packets []*packet, windowsize, shift, blockNumber, offset int) error {
	stop := shift + windowsize
	if stop >= blockNumber {
		stop = blockNumber
	}

	start := blockNumber - offset
	if start < 0 {
		start = 0
	} else if start > blockNumber {
		return nil
	}

	var acked int
	for i := start; i < stop; i++ {
		if !packets[i].acked {
			if time.Since(packets[i].lastSend) >= timeWait {
				if packets[i].attempts > 0 {
					MetricRetransmitMessages.Inc()
				}
				if packets[i].attempts == maxSendAttempts {
					MetricExpiredMessages.Inc()
					return ErrMaxAttempts
				}
				packets[i].attempts++
				packets[i].lastSend = time.Now()
				if err := sr.sendToSocket(packets[i].message); err != nil {
					return err
				}
			}
		} else {
			acked++
		}
	}

	if len(packets) == stop {
		if time.Since(packets[len(packets)-1].lastSend) >= timeWait {
			MetricExpiredMessages.Inc()
			return ErrMaxAttempts
		}
	}

	return nil
}

func (sr *transport) sendPacketsByWindowOffsetToAddr(packets []*packet, windowsize, shift, blockNumber, offset int, addr net.Addr) error {
	stop := shift + windowsize
	if stop >= blockNumber {
		stop = blockNumber
	}

	start := blockNumber - offset
	if start < 0 {
		start = 0
	} else if start > blockNumber {
		return nil
	}

	var acked int
	for i := start; i < stop; i++ {
		if !packets[i].acked {
			if time.Since(packets[i].lastSend) >= timeWait {
				if packets[i].attempts > 0 {
					MetricRetransmitMessages.Inc()
				}
				if packets[i].attempts == maxSendAttempts {
					MetricExpiredMessages.Inc()
					return ErrMaxAttempts
				}
				packets[i].attempts++
				packets[i].lastSend = time.Now()
				if err := sr.sendToSocketByAddress(packets[i].message, addr); err != nil {
					return err
				}
			}
		} else {
			acked++
		}
	}

	if len(packets) == stop {
		if time.Since(packets[len(packets)-1].lastSend) >= timeWait {
			MetricExpiredMessages.Inc()
			return ErrMaxAttempts
		}
	}

	return nil
}

func (sr *transport) sendPacketsToAddr(packets []*packet, windowsize int, shift int, addr net.Addr) error {
	stop := shift + windowsize
	if stop >= len(packets) {
		stop = len(packets)
	}

	// need a more elegant solution
	if shift == len(packets) {
		return ErrMaxAttempts
	}

	var acked int
	for i := 0; i < stop; i++ {
		if !packets[i].acked {
			if time.Since(packets[i].lastSend) >= timeWait {
				if packets[i].attempts == maxSendAttempts {
					MetricExpiredMessages.Inc()
					return ErrMaxAttempts
				}
				packets[i].attempts++
				packets[i].lastSend = time.Now()
				if err := sr.sendToSocketByAddress(packets[i].message, addr); err != nil {
					return err
				}
			}
		} else {
			acked++
		}
	}

	return nil
}

func (sr *transport) sendARQBlock1CON(message *CoAPMessage) (*CoAPMessage, error) {
	state := new(stateSend)
	state.payload = message.Payload.Bytes()
	state.lenght = len(state.payload)
	state.origMessage = message
	state.blockSize = MAX_PAYLOAD_SIZE
	numblocks := math.Ceil(float64(state.lenght) / float64(MAX_PAYLOAD_SIZE))
	if numblocks < DEFAULT_WINDOW_SIZE {
		state.windowsize = int(numblocks)
	} else {
		state.windowsize = DEFAULT_WINDOW_SIZE
	}

	packets := []*packet{}

	for {
		blockMessage, end := constructNextBlock(OptionBlock1, state)
		packets = append(packets, &packet{
			acked:   false,
			message: blockMessage,
		})

		if end {
			break
		}
	}

	var shift = 0

	err := sr.sendPackets(packets, state.windowsize, shift)
	if err != nil {
		return nil, err
	}

	for {
		resp, err := receiveMessage(sr, message)
		if err != nil {
			if err == ErrMaxAttempts {
				if err = sr.sendPackets(packets, state.windowsize, shift); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}

		if resp.Type == ACK {
			if resp.Type == ACK && resp.Code == CoapCodeEmpty {
				return sr.receiveARQBlock2(message, nil)
			}

			if resp.GetBlock2() != nil {
				return sr.receiveARQBlock2(message, resp)
			}

			block := resp.GetBlock1()
			if block != nil {
				wo := resp.GetOption(OptionWindowtOffset)
				if wo != nil {
					sr.sendPacketsByWindowOffset(packets, state.windowsize, shift, block.BlockNumber, int(wo.Value.(uint16)))
				}
				if len(packets) >= block.BlockNumber {
					if resp.Code != CoapCodeContinue {
						return resp, nil
					}
					packets[block.BlockNumber].acked = true
					if block.BlockNumber == shift {
						shift++
						for _, p := range packets[shift:] {
							if p.acked {
								shift++
							} else {
								break
							}
						}

						if err = sr.sendPackets(packets, state.windowsize, shift); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
}

func (sr *transport) sendARQBlock2ACK(input chan *CoAPMessage, message *CoAPMessage, addr net.Addr) error {
	state := new(stateSend)
	state.payload = message.Payload.Bytes()
	state.lenght = len(state.payload)
	state.origMessage = message
	state.blockSize = MAX_PAYLOAD_SIZE
	numblocks := math.Ceil(float64(state.lenght) / float64(MAX_PAYLOAD_SIZE))
	if numblocks < DEFAULT_WINDOW_SIZE {
		state.windowsize = int(numblocks)
	} else {
		state.windowsize = DEFAULT_WINDOW_SIZE
	}

	packets := []*packet{}

	emptyAckMessage := newACKEmptyMessage(message, state.windowsize)
	err := sr.sendToSocketByAddress(emptyAckMessage, addr)
	if err != nil {
		return err
	}
	emptyAckMessage = nil

	for {
		blockMessage, end := constructNextBlock(OptionBlock2, state)
		packets = append(packets, &packet{
			acked:   false,
			message: blockMessage,
		})

		if end {
			break
		}
	}

	var shift = 0

	if err := sr.sendPacketsToAddr(packets, state.windowsize, shift, addr); err != nil {
		return err
	}

	for {
		select {
		case resp := <-input:
			if !bytes.Equal(resp.Token, message.Token) {
				continue
			}
			if resp.Type == ACK {
				block := resp.GetBlock2()
				if block != nil {
					if len(packets) >= block.BlockNumber {
						if resp.Code != CoapCodeContinue {
							return nil
						}
						if block.BlockNumber < len(packets) {
							wo := resp.GetOption(OptionWindowtOffset)
							if wo != nil {
								wov, ok := wo.Value.(uint16)
								if ok {
									sr.sendPacketsByWindowOffset(packets, state.windowsize, shift, block.BlockNumber, int(wov))
								}
							}

							packets[block.BlockNumber].acked = true
							if block.BlockNumber == shift {
								shift++
								for _, p := range packets[shift:] {
									if p.acked {
										shift++
									} else {
										break
									}
								}

								if err := sr.sendPacketsToAddr(packets, state.windowsize, shift, addr); err != nil {
									return err
								}
							}
						}
					}
				}
			}
		case <-time.After(sumTimeAttempts):
			if err := sr.sendPacketsToAddr(packets, state.windowsize, shift, addr); err != nil {
				return err
			}
		}
	}
}

func (sr *transport) receiveARQBlock1(input chan *CoAPMessage) (*CoAPMessage, error) {
	buf := make(map[int][]byte)
	totalBlocks := -1

	for {
		select {
		case inputMessage := <-input:
			block := inputMessage.GetBlock1()
			if block == nil || inputMessage.Type != CON {
				continue
			}
			if !block.MoreBlocks {
				totalBlocks = block.BlockNumber + 1
			}

			buf[block.BlockNumber] = inputMessage.Payload.Bytes()
			if totalBlocks == len(buf) {
				b := []byte{}
				for i := 0; i < totalBlocks; i++ {
					b = append(b, buf[i]...)
				}
				inputMessage.Payload = NewBytesPayload(b)

				return inputMessage, nil
			}

			var ack *CoAPMessage
			w := inputMessage.GetOption(OptionSelectiveRepeatWindowSize)
			if w != nil {
				ack = ackToWithWindowOffset(nil, inputMessage, CoapCodeContinue, w.IntValue(), block.BlockNumber, buf)
			} else {
				ack = ackTo(nil, inputMessage, CoapCodeContinue)
			}

			if err := sr.sendToSocketByAddress(ack, inputMessage.Sender); err != nil {
				return nil, err
			}

		case <-time.After(sumTimeAttempts):
			MetricExpiredMessages.Inc()
			return nil, ErrMaxAttempts
		}
	}
}

func (sr *transport) receiveARQBlock2(origMessage *CoAPMessage, inputMessage *CoAPMessage) (rsp *CoAPMessage, err error) {
	buf := make(map[int][]byte)
	totalBlocks := -1

	var attempts int

	if inputMessage != nil {
		block := inputMessage.GetBlock2()
		if block != nil && inputMessage.Type == CON {
			if !block.MoreBlocks {
				totalBlocks = block.BlockNumber + 1
			}
			buf[block.BlockNumber] = inputMessage.Payload.Bytes()
			if totalBlocks == len(buf) {
				b := []byte{}
				for i := 0; i < totalBlocks; i++ {
					b = append(b, buf[i]...)
				}
				inputMessage.Payload = NewBytesPayload(b)

				ack := ackTo(origMessage, inputMessage, CoapCodeEmpty)
				sr.sendToSocket(ack)
				return inputMessage, nil
			}

			var ack *CoAPMessage
			w := inputMessage.GetOption(OptionSelectiveRepeatWindowSize)
			if w != nil {
				ack = ackToWithWindowOffset(origMessage, inputMessage, CoapCodeContinue, w.IntValue(), block.BlockNumber, buf)
			} else {
				ack = ackTo(origMessage, inputMessage, CoapCodeContinue)
			}
			sr.sendToSocket(ack)
		}
	}

	for {
		inputMessage, err = receiveMessage(sr, origMessage)
		if err == ErrMaxAttempts {
			if attempts == maxSendAttempts {
				MetricExpiredMessages.Inc()
				return nil, err
			}
			attempts++
			continue
		}
		if err != nil {
			return nil, err
		}

		if attempts > 0 {
			MetricRetransmitMessages.Inc()
		}
		block := inputMessage.GetBlock2()
		if block == nil || inputMessage.Type != CON {
			continue
		}

		if !block.MoreBlocks {
			totalBlocks = block.BlockNumber + 1
		}

		buf[block.BlockNumber] = inputMessage.Payload.Bytes()
		if totalBlocks == len(buf) {
			b := []byte{}
			for i := 0; i < totalBlocks; i++ {
				b = append(b, buf[i]...)
			}
			inputMessage.Payload = NewBytesPayload(b)
			ack := ackTo(origMessage, inputMessage, CoapCodeEmpty)
			if err = sr.sendToSocket(ack); err != nil {
				return nil, err
			}
			return inputMessage, nil
		}

		var ack *CoAPMessage
		w := inputMessage.GetOption(OptionSelectiveRepeatWindowSize)
		if w != nil {
			ack = ackToWithWindowOffset(origMessage, inputMessage, CoapCodeContinue, w.IntValue(), block.BlockNumber, buf)
		} else {
			ack = ackTo(origMessage, inputMessage, CoapCodeContinue)
		}

		if err = sr.sendToSocket(ack); err != nil {
			return nil, err
		}
	}
}

func (sr *transport) ReceiveMessage(message *CoAPMessage, respHandler func(*CoAPMessage, error)) {
	message, err := preparationReceivingMessage(sr, message)
	if err != nil {
		return
	}

	sr.messageHandlerSelector(message, respHandler)
}

func (sr *transport) ReceiveOnce(respHandler func(*CoAPMessage, error)) {
	readBuf := make([]byte, MTU+1)
start:
	n, senderAddr, err := sr.conn.Listen(readBuf)
	if err != nil {
		panic(err)
	}
	if n == 0 || n > MTU {
		goto start
	}

	message, err := preparationReceivingBuffer("receiveOnce", sr, readBuf[:n], senderAddr, "")
	if err != nil {
		goto start
	}

	message.Sender = senderAddr

	if _, ok := handlersStateCache.Get(senderAddr.String() + string(message.Token)); ok {
		return
	}

	handlersStateCache.SetDefault(senderAddr.String()+string(message.Token), struct{}{})
	sr.messageHandlerSelector(message, respHandler)
	handlersStateCache.Delete(message.Sender.String() + string(message.Token))
}

func (sr *transport) messageHandlerSelector(message *CoAPMessage, respHandler func(*CoAPMessage, error)) {
	block1 := message.GetBlock1()
	block2 := message.GetBlock2()

	id := message.Sender.String() + string(message.Token)
	var (
		c  interface{}
		ch chan *CoAPMessage
		ok bool
	)

	if block1 != nil {
		c, ok = sr.block1channels.Load(id)
		if !ok {
			if message.Type == CON {
				ch = make(chan *CoAPMessage, 102400)
				go func() {
					resp, err := sr.receiveARQBlock1(ch)
					sr.block1channels.Delete(id)
					respHandler(resp, err)
				}()
				sr.block1channels.Store(id, ch)
				ch <- message
				return
			}
		} else {
			c.(chan *CoAPMessage) <- message
			return
		}
		return
	}

	if block2 != nil {
		if message.Type == ACK {
			c, ok = sr.block2channels.Load(id)
			if ok {
				c.(chan *CoAPMessage) <- message
			}
		}
		return
	}
	go respHandler(message, nil)
}

func preparationSendingMessage(tr *transport, message *CoAPMessage, addr net.Addr) ([]byte, error) {
	secMessage := message.Clone(true)

	if err := securityOutputLayer(tr, secMessage, addr); err != nil {
		return nil, err
	}

	buf, err := Serialize(secMessage)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func preparationReceivingBuffer(tag string, tr *transport, data []byte, senderAddr net.Addr, proxyAddr string) (*CoAPMessage, error) {
	message, err := Deserialize(data)
	if err != nil {
		return nil, err
	}
	if message == nil {
		return nil, ErrNilMessage
	}

	MetricReceivedMessages.Inc()

	message.Sender = senderAddr
	// fmt.Println(time.Now().Format("15:04:05.000000000"), "\t<--- ", tag, " receive\t", senderAddr, message.ToReadableString())

	_, err = securityInputLayer(tr, message, proxyAddr)

	if err != nil {
		return nil, err
	}
	return message, nil
}

func preparationReceivingMessage(tr *transport, message *CoAPMessage) (*CoAPMessage, error) {
	// fmt.Println(time.Now().Format("15:04:05.000000000"), "\t<--- receive\t", message.Sender, message.ToReadableString())
	MetricReceivedMessages.Inc()
	_, err := securityInputLayer(tr, message, "")
	if err != nil {
		return nil, err
	}
	return message, nil
}

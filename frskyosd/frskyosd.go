package frskyosd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"time"

	"github.com/fiam/max7456tool/mcm"
	log "github.com/sirupsen/logrus"
	"go.bug.st/serial"
)

var (
	// ErrTimeout is returned when a method expecting a response
	// times out
	ErrTimeout = errors.New("timeout")
)

const (
	// Workaround cosmetic bug in first bootloader version.
	// This doesn't affect functionality, but would show
	// some error messages when it shouldn't
	earlyBootloaderWorkaround = true
)

type unexpectedMessageError struct {
	Expected osdCmd
	Message  message
}

func (e *unexpectedMessageError) Error() string {
	return fmt.Sprintf("expecting reply with command %d, got %d instead (%+v)", e.Expected, e.Message.command(), e.Message)
}

type osdCmd byte

const (
	cmdError      osdCmd = 0
	cmdInfo              = 1
	cmdReadFont          = 2
	cmdWriteFont         = 3
	cmdReboot            = 120
	cmdWriteFlash        = 121
)

const (
	flashWriteMaxSize = 64
	flashWriteEnd     = math.MaxUint32
)

// OSD represents a active connection to an FrSky OSD. Use
// New to start a new connection.
type OSD struct {
	port       serial.Port
	portCh     chan byte
	responseCh chan *frame
}

func (o *OSD) readPort() {
	b := make([]byte, 1)
	for {
		_, err := o.port.Read(b)
		if err != nil {
			log.Printf("error reading from port: %v", err)
			break
		}
		log.Tracef(o.dumpByte("R <<", b[0]))
		o.portCh <- b[0]
	}
	close(o.portCh)
}

func (o *OSD) write(data []byte) error {
	for _, b := range data {
		log.Tracef(o.dumpByte("W >>", b))
	}

	_, err := o.port.Write(data)
	return err
}

func (o *OSD) send(cmd osdCmd, data []byte) error {
	log.Debugf("%d=> %s\n", cmd, hex.EncodeToString(data))
	var buf bytes.Buffer
	buf.WriteByte('$')
	buf.WriteByte('A')
	payload := make([]byte, 0, 1+len(data))
	payload = append(payload, byte(cmd))
	payload = append(payload, data...)
	sz := byte(len(payload))
	buf.WriteByte(sz)
	buf.Write(payload)

	cs := newCrc8D5Checksum()
	cs.WriteByte(sz)
	checkSumWrite(cs, payload)
	buf.WriteByte(cs.Sum8())

	return o.write(buf.Bytes())
}

func (o *OSD) awaitResponse() (message, error) {
	for {
		select {
		case resp := <-o.responseCh:
			msg := getMessage(resp.Type, resp.Cmd)
			if msg == nil {
				log.Warnf("dropping unknown message %+v\n", resp)
				continue
			}
			if err := msg.decode(resp.Cmd, resp.Payload); err != nil {
				return nil, fmt.Errorf("error decoding message %d: %v", resp.Cmd, err)
			}
			if msg.frameType() == frameTypeMSP && msg.command() == int(mspCmdLog) {
				logMessage := msg.(*mspLogMessage)
				log.Infof("MSP LOG: %s", logMessage.Message)
				continue
			}
			return msg, nil
		case <-time.After(1 * time.Second):
			return nil, ErrTimeout
		}
	}
}

// Info returns the OSD hardware and configuration information.
// See InfoMessage for more details.
func (o *OSD) Info() (*InfoMessage, error) {
	return o.info(true)
}

func (o *OSD) info(tryMspPassthrough bool) (*InfoMessage, error) {
	if err := o.send(cmdInfo, []byte{1}); err != nil {
		return nil, err
	}
	msg, err := o.awaitResponse()
	if err != nil {
		if tryMspPassthrough && err == ErrTimeout {
			ok, mspErr := o.setupMspPassthrough()
			if ok {
				// Try again
				return o.info(false)
			}
			if mspErr != nil {
				return nil, mspErr
			}
		}
		return nil, err
	}
	if info, ok := msg.(*InfoMessage); ok {
		return info, nil
	}
	return nil, &unexpectedMessageError{Expected: cmdInfo, Message: msg}
}

// ReadFontChar reads the character at the given index from the
// non volatile font stored in the OSD.
func (o *OSD) ReadFontChar(idx uint) (*FontCharMessage, error) {
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf, uint16(idx))
	if err := o.send(cmdReadFont, buf); err != nil {
		return nil, err
	}
	msg, err := o.awaitResponse()
	if err != nil {
		return nil, err
	}
	return msg.(*FontCharMessage), nil
}

// WriteFontChar writes a font character with the given data and index
// to the non volatile font memory. The data must be in MCM format and be
// either 54 (just character visible data) or 64 (visible data + metadata) bytes.
func (o *OSD) WriteFontChar(idx uint, data []byte) error {
	if len(data) != mcm.MinCharBytes && len(data) != mcm.CharBytes {
		return fmt.Errorf("invalid char data size %d - must be %d or %d", len(data), mcm.MinCharBytes, mcm.CharBytes)
	}
	buf := make([]byte, 2+len(data))
	binary.LittleEndian.PutUint16(buf, uint16(idx))
	copy(buf[2:], data)
	if err := o.send(cmdWriteFont, buf); err != nil {
		return err
	}
	_, err := o.awaitResponse()
	if err != nil {
		return err
	}
	return nil
}

// UploadFont updates the whole font in the OSD. The data must be an .mcm file.
func (o *OSD) UploadFont(r io.Reader, progress func(done int, total int)) error {
	dec, err := mcm.NewDecoder(r)
	if err != nil {
		return err
	}
	total := dec.NChars()
	for ii := 0; ii < total; ii++ {
		chr := dec.CharAt(ii)
		if err := o.WriteFontChar(uint(ii), chr.Data()); err != nil {
			return err
		}
		if progress != nil {
			progress(ii+1, total)
		}
	}
	return nil
}

// FlashFirmware flashes the given firmware to the OSD. The data must be
// an FrSky supplied firmware file. Alternatively, a nil io.Reader can be
// passsed to erase the whole firmware and leave only the bootloader.
func (o *OSD) FlashFirmware(r io.Reader, progress func(done int, total int)) error {
	var data []byte
	var err error
	if r != nil {
		data, err = ioutil.ReadAll(r)
		if err != nil {
			return err
		}
	}
	if err := o.reboot(true); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	info, err := o.Info()
	if err != nil {
		return err
	}
	if !info.IsBootloader {
		return errors.New("failed to reboot into bootloader mode")
	}
	if err := o.flashBegin(); err != nil {
		return err
	}
	rem := data
	addr := uint32(0)
	for len(rem) > 0 {
		n := len(rem)
		if n > flashWriteMaxSize {
			n = flashWriteMaxSize
		}
		chunk := rem[:n]
		rem = rem[n:]
		next, err := o.flashChunk(addr, chunk)
		if err != nil {
			if earlyBootloaderWorkaround {
				if len(rem) == 0 {
					if ue, ok := err.(*unexpectedMessageError); ok {
						if em, ok := ue.Message.(*ErrorMessage); ok && em.Cmd == cmdWriteFlash {
							next = addr + uint32(n)
						}
					}
				}
			}
			if next == 0 {
				return err
			}
		}
		addr += uint32(n)
		if next != addr {
			return fmt.Errorf("expecting next addr = %d, got %d instead", addr, next)
		}
		if progress != nil {
			progress(int(addr), len(data))
		}
	}
	if err := o.flashEnd(); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	if err := o.reboot(false); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	info, err = o.Info()
	if err != nil {
		return err
	}
	if info.IsBootloader {
		return errors.New("failed to reboot into osd mode")
	}
	return nil
}

// Close closes the connection to the OSD
func (o *OSD) Close() error {
	return o.port.Close()
}

func (o *OSD) flashChunk(addr uint32, data []byte) (uint32, error) {
	payload := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(payload, addr)
	if len(data) > 0 {
		copy(payload[4:], data)
	}
	if err := o.send(cmdWriteFlash, payload); err != nil {
		return 0, err
	}
	msg, err := o.awaitResponse()
	if err != nil {
		return 0, err
	}
	raw, ok := msg.(*RawMessage)
	if !ok || raw.Cmd != cmdWriteFlash {
		return 0, &unexpectedMessageError{
			Expected: cmdWriteFlash,
			Message:  msg,
		}
	}
	return binary.LittleEndian.Uint32(raw.Payload), nil
}

func (o *OSD) flashBegin() error {
	addr, err := o.flashChunk(0, nil)
	if addr != 0 {
		return fmt.Errorf("begin flash returned offset %v instead of 0", addr)
	}
	return err
}

func (o *OSD) flashEnd() error {
	_, err := o.flashChunk(flashWriteEnd, nil)
	if err != nil && earlyBootloaderWorkaround {
		if _, ok := err.(*unexpectedMessageError); ok {
			err = nil
		}
	}
	return err
}

func (o *OSD) reboot(toBootloader bool) error {
	data := []byte{0}
	if toBootloader {
		data[0] = 1
	}
	return o.send(cmdReboot, data)
}

func (o *OSD) dumpByte(prefix string, b byte) string {
	s := string([]byte{b})
	return fmt.Sprintf("%s %03d = 0x%02x = %q\n", prefix, b, b, s)
}

// New returns an initialized OSD given its port name.
func New(port string) (*OSD, error) {
	mode := &serial.Mode{
		BaudRate: 115200,
	}
	p, err := serial.Open(portName(port), mode)
	if err != nil {
		return nil, err
	}
	osd := &OSD{
		port:       p,
		portCh:     make(chan byte, 512),
		responseCh: make(chan *frame, 8),
	}
	go osd.readPort()
	go osd.decodeResponses()
	return osd, nil
}

// AvailablePorts returns the list of ports in the system
// that can be used to connect to FrSkyOSD
func AvailablePorts() ([]string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		if pe, ok := err.(*serial.PortError); ok {
			if pe.Code() == serial.ErrorEnumeratingPorts {
				// This happens on Windows when there are
				// no serial ports
				return nil, nil
			}
		}
		return nil, err
	}
	return filterPorts(ports), nil
}

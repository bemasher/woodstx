package main

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

const (
	grpA = iota
	grpB
	grpC
	grpD
)

const (
	addr1 = iota
	addr2
	addr3
)

const (
	// Symbols are all 2 bits each. Lengths are in symbols.
	grpLen  = 4
	addrLen = 3
	padLen  = 4

	// Everything is padded with this symbol.
	padSym = "01"

	// Messages are each repeated 5 times with a "repeat" bit appended.
	repeats = 5

	sampleRate = 150000
	bitRate    = 751
	symLen     = sampleRate / bitRate

	// This math is kind of silly, but it's necessary to make sure these
	// constants are untyped integers rather than floating point values.
	blankLen = 6*symLen + symLen>>1 // Samples between repeated messages (6.5 symbol lengths)
	flushLen = 32000                // Samples to flush sendiq's FIFO.

	dutyCycle = 3 // 0.3 = 3 / 10

	// 0 bits are a short pulse (30% of symbol length) and a long pause.
	bit0PulseLen = symLen * dutyCycle / 10 // x * 3 / 10 = x * 0.3
	bit0PauseLen = symLen - bit0PulseLen

	// 1 bits are a long pulse (70% of symbol length) and a short pause.
	bit1PulseLen = bit0PauseLen
	bit1PauseLen = bit0PulseLen
)

// Messages consist of a group A-D, an address 1-3 and a state, on or off.
type Message struct {
	Group int
	Addr  int
	State bool
}

func (msg Message) String() (s string) {
	s = string(msg.Group + 'A')
	s += string(msg.Addr + '1')

	if msg.State {
		return s + "+"
	}

	return s + "-"
}

// A1+: 00______00____________11
// A1-: 00______00____________00
// A2+: 00________00__________11
// A2-: 00________00__________00
// A3+: 00__________00________11
// A3-: 00__________00________00
// B1+: __00____00____________11
// B1-: __00____00____________00
// B2+: __00______00__________11
// B2-: __00______00__________00
// B3+: __00________00________11
// B3-: __00________00________00
// C1+: ____00__00____________11
// C1-: ____00__00____________00
// C2+: ____00____00__________11
// C2-: ____00____00__________00
// C3+: ____00______00________11
// C3-: ____00______00________00
// D1+: ______0000____________11
// D1-: ______0000____________00
// D2+: ______00__00__________11
// D2-: ______00__00__________00
// D3+: ______00____00________11
// D3-: ______00____00________00

func (msg Message) BitString() (s string) {
	var buf strings.Builder

	// Write group symbols.
	msg.writeBitGroup(grpLen, msg.Group, &buf)

	// Write address symbols.
	msg.writeBitGroup(addrLen, msg.Addr, &buf)

	for idx := 0; idx < padLen; idx++ {
		buf.WriteString(padSym)
	}

	if msg.State {
		buf.WriteString("11")
	} else {
		buf.WriteString("00")
	}

	return buf.String()
}

// A bit group is a series of padding symbols with a '00' placed at the index indicated.
func (msg Message) writeBitGroup(length, index int, buf *strings.Builder) {
	for idx := 0; idx < length; idx++ {
		if idx == index {
			buf.WriteString("00")
		} else {
			buf.WriteString(padSym)
		}
	}
}

// Given a message and a buffer, write IQ samples representing the message to be transmitted.
func (msg Message) WriteIQ(buf *bytes.Buffer) {
	WriteSymbol(0, flushLen, buf)

	// Get the message's bits.
	bitString := msg.BitString()

	// Messages are repeated times.
	for repeat := repeats; repeat >= 0; repeat-- {
		// For each bit in the message.
		for _, bit := range bitString {
			// Write the appropriate bit to the IQ sample buffer.
			switch bit {
			case '0':
				WriteSymbol(bit0PulseLen, bit0PauseLen, buf)
			case '1':
				WriteSymbol(bit1PulseLen, bit1PauseLen, buf)
			}
		}
		// Repeated messages have an extra bit to indicate another message is comming.
		if repeat > 0 {
			WriteSymbol(bit0PulseLen, bit0PauseLen, buf)
			WriteSymbol(0, blankLen, buf)
		}
	}

	// Write enough samples to flush sendiq's buffer.
	WriteSymbol(0, flushLen, buf)
}

// Give pulse and pause lengths, write IQ samples to the buffer.
func WriteSymbol(pulseLen, pauseLen int, buf *bytes.Buffer) {
	for idx := 0; idx < pulseLen; idx++ {
		// The math works out such that if we want to transmit a pulse at full
		// amplitude at exactly 0 Hz, the in-phase component should be 1 and the
		// quadrature component should be 0. To translate samples to uint8's
		// use: x * 127.5 + 127.5 (integer math of course).
		buf.Write([]byte{255, 127})
	}

	// The pause consists of 0 amplitude samples, which comes out to 127 for uint8's.
	for idx := 0; idx < pauseLen; idx++ {
		buf.Write([]byte{127, 127})
	}
}

// Template data consists of a list of groups and addresses.
type TemplateData struct {
	Groups    []string
	Addresses []string
}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

func main() {
	// There is likely a safer and more robust way to match the url for the API
	// calls, but I can't be bothered and this is secure enough given the
	// environments this tool is intended to run in.
	urlRe := regexp.MustCompile(`^/api/([A-D])([1-3])([+\-])$`)

	indexTmpl, err := template.ParseFiles("index.html")
	if err != nil {
		log.Fatal(errors.Wrap(err, "parse template"))
	}

	var tmplData TemplateData
	for group := 'A'; group <= 'D'; group++ {
		tmplData.Groups = append(tmplData.Groups, string(group))
	}
	for address := '1'; address <= '3'; address++ {
		tmplData.Addresses = append(tmplData.Addresses, string(address))
	}

	iqBuf := new(bytes.Buffer)
	msgCh := make(chan Message)

	// Fill sendiq's buffer so it does not transmit it's default carrier.
	WriteSymbol(0, flushLen, iqBuf)
	iqBuf.WriteTo(os.Stdout)

	// In a goroutine, start listening for messages from the web interface,
	// generate and write IQ samples to stdout when we receive them.
	go func() {
		for msg := range msgCh {
			msg.WriteIQ(iqBuf)
			iqBuf.WriteTo(os.Stdout)
		}
	}()

	// Serve the index.html template.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		indexTmpl.Execute(w, tmplData)
	})

	// Serve assets such as js and css.
	http.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("./assets"))))

	// Serve api calls to transmit messages.
	http.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.EscapedPath()

		// If the url doesn't match the regex, it's invalid.
		if !urlRe.MatchString(u) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Extract the group, address and state characters from the url.
		submatch := urlRe.FindStringSubmatch(u)
		msgCh <- Message{
			Group: int(submatch[1][0] - 'A'), // Groups are 0-indexed, so subtract 'A'.
			Addr:  int(submatch[2][0] - '1'), // Addresses are 0-indexed, so subtract '1'.
			State: submatch[3][0] == '+',     // The on state is represented by a '+'.
		}

		w.WriteHeader(http.StatusOK)
	})

	http.ListenAndServe(":8080", nil)
}

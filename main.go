package main

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

// A1+: 00______00____________110
// A1-: 00______00____________000
// A2+: 00________00__________110
// A2-: 00________00__________000
// A3+: 00__________00________110
// A3-: 00__________00________000
// B1+: __00____00____________110
// B1-: __00____00____________000
// B2+: __00______00__________110
// B2-: __00______00__________000
// B3+: __00________00________110
// B3-: __00________00________000
// C1+: ____00__00____________110
// C1-: ____00__00____________000
// C2+: ____00____00__________110
// C2-: ____00____00__________000
// C3+: ____00______00________110
// C3-: ____00______00________000
// D1+: ______0000____________110
// D1-: ______0000____________000
// D2+: ______00__00__________110
// D2-: ______00__00__________000
// D3+: ______00____00________110
// D3-: ______00____00________000

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

	padSym = "01"

	repeats = 5

	sampleRate = 150000
	bitRate    = 751
	symLen     = sampleRate / bitRate
	blankLen   = 6*symLen + symLen>>1 // Samples between repeated messages.
	flushLen   = 32000                // Samples to flush sendiq's FIFO.

	dutyCycle    = 3                         // 0.3 = 3 / 10
	bit0PulseLen = (symLen * dutyCycle) / 10 // x * 3 / 10 = x * 0.3
	bit0PauseLen = symLen - bit0PulseLen

	bit1PulseLen = bit0PauseLen
	bit1PauseLen = bit0PulseLen
)

type Message struct {
	Group int
	Addr  int
	State bool
}

func (msg Message) String() (s string) {
	switch msg.Group {
	case grpA:
		s = "A"
	case grpB:
		s = "B"
	case grpC:
		s = "C"
	case grpD:
		s = "D"
	}

	switch msg.Addr {
	case addr1:
		s += "1"
	case addr2:
		s += "2"
	case addr3:
		s += "3"
	}

	if msg.State {
		return s + "+"
	}

	return s + "-"
}

func (msg Message) BitString() (s string) {
	var buf strings.Builder

	// Write group symbols.
	msg.writeBitGroup(grpLen, msg.Group, &buf)

	// Write group symbols.
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

func (msg Message) writeBitGroup(length, index int, buf *strings.Builder) {
	for idx := 0; idx < length; idx++ {
		if idx == index {
			buf.WriteString("00")
		} else {
			buf.WriteString(padSym)
		}
	}
}

func (msg Message) WriteIQ(buf *bytes.Buffer) {
	WriteSymbol(0, flushLen, buf)

	bitString := msg.BitString()

	for repeat := repeats; repeat >= 0; repeat-- {
		for _, bit := range bitString {
			switch bit {
			case '0':
				WriteSymbol(bit0PulseLen, bit0PauseLen, buf)
			case '1':
				WriteSymbol(bit1PulseLen, bit1PauseLen, buf)
			}
		}
		if repeat > 0 {
			WriteSymbol(bit0PulseLen, bit0PauseLen, buf)
			WriteSymbol(0, blankLen, buf)
		}
	}

	WriteSymbol(0, flushLen, buf)
}

func WriteSymbol(pulseLen, pauseLen int, buf *bytes.Buffer) {
	for idx := 0; idx < pulseLen; idx++ {
		buf.Write([]byte{255, 127})
	}
	for idx := 0; idx < pauseLen; idx++ {
		buf.Write([]byte{127, 127})
	}
}

type TemplateData struct {
	Groups    []string
	Addresses []string
}

func (td TemplateData) Url(group, address string, state bool) template.URL {
	if state {
		return template.URL("/api/" + group + address + "+")
	}

	return template.URL("/api/" + group + address + "-")
}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

func main() {
	urlRe := regexp.MustCompile(`^/api/([A-D])([1-3])([+\-])$`)

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

	go func() {
		for msg := range msgCh {
			msg.WriteIQ(iqBuf)
			iqBuf.WriteTo(os.Stdout)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		indexTmpl, err := template.ParseFiles("index.html")
		if err != nil {
			fmt.Fprintln(w, errors.Wrap(err, "parse template"))
			return
		}

		indexTmpl.Execute(w, tmplData)
	})

	// http.Handle("/assets", http.FileServer(http.Dir("assets")))
	http.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("./assets"))))

	http.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.EscapedPath()

		if !urlRe.MatchString(u) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		submatch := urlRe.FindStringSubmatch(u)
		msg := Message{
			Group: int(submatch[1][0] - 'A'),
			Addr:  int(submatch[2][0] - '1'),
			State: submatch[3][0] == '+',
		}
		msgCh <- msg

		w.WriteHeader(http.StatusOK)
	})

	http.ListenAndServe(":8080", nil)
}

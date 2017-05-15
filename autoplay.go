package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"text/template"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/kr/pty"
	"golang.org/x/crypto/ssh/terminal"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

const VERSION = "0.0.1"

type DataType int

const (
	INPUT DataType = iota
	OUTPUT
	OUTCODE
)

type Data struct {
	Type  DataType
	Bytes []byte
}

type DataRecorder struct {
	Data    []Data
	Squash  []string
	Split   []string
	Imports []string
}

func (w *DataRecorder) Write(p []byte) (n int, err error) {
	tmp := make([]byte, len(p))
	copy(tmp, p)
	w.Data = append(w.Data, Data{OUTPUT, tmp})
	return os.Stdout.Write(p)
}

func (w *DataRecorder) AddImport(add string) {
	for _, imp := range w.Imports {
		if imp == add {
			return
		}
	}
	w.Imports = append(w.Imports, add)
}

// ProcessedData will first merge all the sequential OUTPUT chunks together then will
// split on Split and squash the Squash strings. We do this so that we can create
// autoplay code through repeated runs of the recorder.  Without this variance will
// typically occur due to buffering within the pty output.
func (w *DataRecorder) ProcessedData() []Data {
	// first we merge all the sequential OUTPUT chunks
	processed := []Data{}
	for _, data := range w.Data {
		if len(processed) == 0 {
			processed = append(processed, data)
			continue
		}
		cursor := len(processed) - 1
		prevData := processed[cursor]
		if data.Type == OUTPUT && prevData.Type == OUTPUT {
			prevData.Bytes = append(prevData.Bytes, data.Bytes...)
			processed[cursor] = prevData
		} else {
			processed = append(processed, data)
		}
	}

	replaceRecord := func(i int, data []Data) int {
		if i == 0 {
			processed = append(data, processed[i+1:]...)
		} else if i == len(processed)-1 {
			// last element?
			processed = append(processed[:i], data...)
		} else {
			processed = append(processed[:i], append(data, processed[i+1:]...)...)
		}
		// we need to move the cursor to the end of the elements we just replaced
		return i + len(data) - 1
	}

	// look for records to split on, this helps keep the line-lengths down
	// in the generated code to make it more readable.
	for _, split := range w.Split {
		// if user used --split "\n" we will see the string "\\n" so we need to
		// unescape that string and turn it into the rune '\n'
		split, err := strconv.Unquote("\"" + split + "\"")
		if err != nil {
			panic(err)
		}
		sequence := []byte(split)
		for i := 0; i < len(processed); i++ {
			data := processed[i]
			if data.Type == INPUT {
				continue
			}
			chunks := bytes.SplitAfter(data.Bytes, sequence)
			if len(chunks) > 1 {
				tmp := []Data{}
				for _, chunk := range chunks {
					if len(chunk) > 0 {
						tmp = append(tmp, Data{OUTPUT, chunk})
					}
				}
				i = replaceRecord(i, tmp)
			}
		}
	}

	// now replace long repeated sequences with some code that will generate
	// the long sequences.  This will help simply the generated code
	for _, squash := range w.Squash {
		// if user used --squash "\r" we will see the string "\\r" so we need to
		// unescape that string and turn it into the rune '\r'
		squash, err := strconv.Unquote("\"" + squash + "\"")
		if err != nil {
			panic(err)
		}
		sequence := []byte(squash)
		for i := 0; i < len(processed); i++ {
			data := processed[i]
			if data.Type == INPUT {
				continue
			}
			chunks := bytes.SplitAfter(data.Bytes, sequence)
			if len(chunks) > 1 {
				tmp := []Data{}
				tally := 0
				for _, chunk := range chunks {
					if bytes.Equal(chunk, sequence) {
						tally++
						continue
					} else {
						if tally > 0 {
							tmp = append(tmp, Data{OUTCODE, []byte(fmt.Sprintf(`strings.Repeat(%q, %d)`, sequence, tally))})
							w.AddImport("strings")
							tally = 0
						}
						if len(chunk) > 0 {
							tmp = append(tmp, Data{OUTPUT, chunk})
						}
					}
				}
				i = replaceRecord(i, tmp)
			}
		}
	}

	return processed
}

func main() {
	opts := struct {
		Args     []string
		Name     string
		Squash   []string
		Split    []string
		Template string
		Cmd      []string
	}{
		Args:  os.Args,
		Split: []string{"\\r\\n"},
	}

	app := kingpin.New("autoplay", "Record program interactions to generate an automated playback program").Version(VERSION)
	app.Flag("name", "File path to write output for generated code").Short('n').Required().StringVar(&opts.Name)
	app.Flag("squash", "squash repeated sequences in recorded output for generated code").Short('q').StringsVar(&opts.Squash)
	app.Flag("split", "sequence to automatically split input on for generated code").Short('s').StringsVar(&opts.Split)
	app.Flag("template", "template used for code generation").Short('t').StringVar(&opts.Template)
	app.Arg("COMMAND", "command to record").Required().StringsVar(&opts.Cmd)
	_, err := app.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		app.Usage([]string{})
		os.Exit(1)
	}

	fh, tty, _ := pty.Open()
	defer tty.Close()
	defer fh.Close()
	rec := &DataRecorder{
		Squash: opts.Squash,
		Split:  opts.Split,
	}
	c := exec.Command(opts.Cmd[0], opts.Cmd[1:]...)
	c.Stdin = tty
	c.Stdout = tty
	c.Stderr = tty
	// start streaming the pty output to our recorder
	go func() {
		io.Copy(rec, fh)
	}()

	// put stdin in raw mode so the terminal will not double-echo and
	// mess up what we are testing
	state, _ := terminal.MakeRaw(syscall.Stdin)
	defer terminal.Restore(syscall.Stdin, state)

	// create a input Buffer so we can read a rune at a time
	inputBuf := bufio.NewReaderSize(os.Stdin, 1024)
	go func() {
		for {
			r, _, err := inputBuf.ReadRune()
			if err != nil {
				break
			}
			b := []byte(string(r))
			rec.Data = append(rec.Data, Data{INPUT, b})
			fh.Write(b)
		}
	}()
	c.Run()
	tty.Close()
	fh.Close()

	// put Stdin back in normal state
	terminal.Restore(syscall.Stdin, state)

	if opts.Template != "" {
		data, err := ioutil.ReadFile(opts.Template)
		if err != nil {
			panic(err)
		}
		DriverTemplate = string(data)
	}

	// generate the autoplay code via template
	results, err := runTemplate(
		DriverTemplate, struct {
			IO               []Data
			Cmd              []string
			Imports          []string
			GeneratorCommand string
		}{
			rec.ProcessedData(),
			opts.Cmd,
			rec.Imports,
			shellquote.Join(opts.Args...),
		},
	)
	if err != nil {
		panic(err)
	}

	out, err := os.Create(opts.Name)
	if err != nil {
		panic(err)
	}
	fmt.Fprint(out, results)
	out.Close()
	exec.Command("gofmt", "-s", "-w", opts.Name).Run()
}

func runTemplate(tmpl string, data interface{}) (string, error) {
	t, err := template.New("code").Funcs(TemplateFuncs).Parse(tmpl)
	if err != nil {
		return "", err
	}
	buf := bytes.NewBufferString("")
	err = t.Execute(buf, data)
	if err != nil {
		return "", err
	}
	return buf.String(), err
}

var TemplateFuncs = map[string]interface{}{
	"INPUT": func() DataType {
		return INPUT
	},
	"OUTPUT": func() DataType {
		return OUTPUT
	},
	"OUTCODE": func() DataType {
		return OUTCODE
	},
}

var DriverTemplate = `
////////////////////////////////////////////////////////////////////////////////
//                          DO NOT MODIFY THIS FILE!
//
//  This file was automatically generated via the commands:
//
//      go get github.com/coryb/autoplay
//      {{.GeneratorCommand}}
//
////////////////////////////////////////////////////////////////////////////////
package main

import (
    "bufio"
    "fmt"
    "os"
    "os/exec"
    "strconv"
    "strings"{{range .Imports}}
	{{ printf "%q" . }}
{{end}}
	"github.com/kr/pty"
)

const (
    RED   = "\033[31m"
    RESET = "\033[0m"
)

func main() {
	fh, tty, _ := pty.Open()
	defer tty.Close()
	defer fh.Close()
	c := exec.Command({{range $i, $v := .Cmd}}{{if gt $i 0}}, {{end}}{{printf "%q" $v}}{{end}})
	c.Stdin = tty
	c.Stdout = tty
	c.Stderr = tty
	c.Start()
	buf := bufio.NewReaderSize(fh, 1024)
{{range .IO}}{{if eq .Type INPUT }}
	fh.Write([]byte({{printf "%q" .Bytes}}))
{{- else if eq .Type OUTCODE }}
	expect({{printf "%s" .Bytes}}, buf)
{{- else }}
	expect({{printf "%q" .Bytes}}, buf)
{{- end}}{{end}}

	c.Wait()
	tty.Close()
	fh.Close()
}

func expect(expected string, buf *bufio.Reader) {
	sofar := []rune{}
	for _, r := range expected {
		got, _, _ := buf.ReadRune()
		sofar = append(sofar, got)
		if got != r {
            fmt.Fprintln(os.Stderr, RESET)

            // we want to quote the string but we also want to make the unexpected character RED
            // so we use the strconv.Quote function but trim off the quoted characters so we can 
            // merge multiple quoted strings into one.
            expStart := strings.TrimSuffix(strconv.Quote(expected[:len(sofar)-1]), "\"")
            expMiss := strings.TrimSuffix(strings.TrimPrefix(strconv.Quote(string(expected[len(sofar)-1])), "\""), "\"")
            expEnd := strings.TrimPrefix(strconv.Quote(expected[len(sofar):]), "\"")

            fmt.Fprintf(os.Stderr, "Expected: %s%s%s%s%s\n", expStart, RED, expMiss, RESET, expEnd)

            // read the rest of the buffer
            p := make([]byte, buf.Buffered())
            buf.Read(p)

            gotStart := strings.TrimSuffix(strconv.Quote(string(sofar[:len(sofar)-1])), "\"")
            gotMiss := strings.TrimSuffix(strings.TrimPrefix(strconv.Quote(string(sofar[len(sofar)-1])), "\""), "\"")
            gotEnd := strings.TrimPrefix(strconv.Quote(string(p)), "\"")

            fmt.Fprintf(os.Stderr, "Got:      %s%s%s%s%s\n", gotStart, RED, gotMiss, RESET, gotEnd)
            panic(fmt.Errorf("Unexpected Rune %q, Expected %q\n", got, r))
        } else {
            fmt.Printf("%c", r)
        }
	}
}
`

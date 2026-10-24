package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
	"golang.org/x/sys/unix"
)

// Client dipakai sapo-api untuk memanggil operasi sapod. Setiap panggilan
// membuka koneksi baru agar sederhana dan tahan terhadap restart sapod.
type Client struct {
	Socket  string
	Timeout time.Duration

	nextID atomic.Uint64
}

// Call menjalankan op dengan params, lalu mendekode hasil ke out (boleh nil).
// Batas waktu bawaan (Timeout) hanya berlaku bila ctx belum punya batas
// waktu; operasi panjang (misalnya membuat pool) memberi batasnya sendiri.
func (c *Client) Call(ctx context.Context, op string, params any, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("sapod tidak bisa dihubungi: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := proto.Request{ID: c.nextID.Add(1), Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), proto.MaxMessageBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return errors.New("sapod menutup koneksi tanpa jawaban")
	}
	var resp proto.Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("jawaban sapod tidak valid: %w", err)
	}
	if resp.ID != req.ID {
		return errors.New("jawaban sapod tidak cocok dengan permintaan")
	}
	if !resp.OK {
		if resp.Error != nil {
			return resp.Error
		}
		return errors.New("sapod mengembalikan error tanpa keterangan")
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// CallFile seperti Call, untuk operasi yang mengembalikan file descriptor
// (FileResult di sapod). Pemanggil wajib menutup file yang dikembalikan.
func (c *Client) CallFile(ctx context.Context, op string, params any, out any) (*os.File, error) {
	if _, ok := ctx.Deadline(); !ok {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("sapod tidak bisa dihubungi: %w", err)
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("koneksi ke sapod bukan unix socket")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = uc.SetDeadline(dl)
	}
	req := proto.Request{ID: c.nextID.Add(1), Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req.Params = raw
	}
	if err := json.NewEncoder(uc).Encode(req); err != nil {
		return nil, err
	}

	// Jawaban dibaca sampai baris baru; file descriptor datang bersama
	// bagian pertamanya.
	var line []byte
	var file *os.File
	buf := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(4))
	for !bytes.Contains(line, []byte{'\n'}) {
		n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
		if oobn > 0 && file == nil {
			if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
				for _, m := range msgs {
					if fds, ferr := unix.ParseUnixRights(&m); ferr == nil {
						for i, fd := range fds {
							if i == 0 && file == nil {
								file = os.NewFile(uintptr(fd), "sapod")
							} else {
								unix.Close(fd)
							}
						}
					}
				}
			}
		}
		line = append(line, buf[:n]...)
		if err != nil || len(line) > proto.MaxMessageBytes {
			if file != nil {
				file.Close()
			}
			if err == nil {
				err = errors.New("jawaban sapod terlalu besar")
			}
			return nil, err
		}
	}
	line = line[:bytes.IndexByte(line, '\n')]
	var resp proto.Response
	err = json.Unmarshal(line, &resp)
	switch {
	case err != nil:
		err = fmt.Errorf("jawaban sapod tidak valid: %w", err)
	case resp.ID != req.ID:
		err = errors.New("jawaban sapod tidak cocok dengan permintaan")
	case !resp.OK && resp.Error != nil:
		err = resp.Error
	case !resp.OK:
		err = errors.New("sapod mengembalikan error tanpa keterangan")
	case file == nil:
		err = errors.New("sapod tidak mengirim file")
	case out != nil && len(resp.Result) > 0:
		err = json.Unmarshal(resp.Result, out)
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return nil, err
	}
	return file, nil
}

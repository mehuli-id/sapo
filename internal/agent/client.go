package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
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

// Copyright 2022 <mzh.scnu@qq.com>. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codec

import (
	"bufio"
	"hash/crc32"
	"io"
	"net/rpc"
	"sync"

	"github.com/zehuamama/tinyrpc/compressor"
	"github.com/zehuamama/tinyrpc/header"
	"github.com/zehuamama/tinyrpc/serializer"
)

type clientCodec struct {
	r io.Reader
	w io.Writer
	c io.Closer

	compressor compressor.CompressType // rpc compress type(raw,gzip,snappy,zlib)
	response   header.ResponseHeader   // rpc response header
	mutex      sync.Mutex              // protect pending map
	pending    map[uint64]string
}

// NewClientCodec Create a new client codec
func NewClientCodec(conn io.ReadWriteCloser,
	compressType compressor.CompressType) rpc.ClientCodec {

	return &clientCodec{
		r:          bufio.NewReader(conn),
		w:          bufio.NewWriter(conn),
		c:          conn,
		compressor: compressType,
		pending:    make(map[uint64]string),
	}
}

// WriteRequest Write the rpc request header and body to the io stream
func (c *clientCodec) WriteRequest(r *rpc.Request, param interface{}) error {
	c.mutex.Lock()
	c.pending[r.Seq] = r.ServiceMethod
	c.mutex.Unlock()
	err := writeRequest(c.w, r, c.compressor, param)
	if err != nil {
		return err
	}
	return nil
}

// ReadResponseHeader read the rpc response header from the io stream
func (c *clientCodec) ReadResponseHeader(r *rpc.Response) error {
	c.response.ResetHeader()
	err := readResponseHeader(c.r, &c.response)
	if err != nil {
		return err
	}
	c.mutex.Lock()
	r.Seq = c.response.ID
	r.Error = c.response.Error
	r.ServiceMethod = c.pending[r.Seq]
	delete(c.pending, r.Seq)
	c.mutex.Unlock()
	return nil
}

// ReadResponseBody read the rpc response body from the io stream
func (c *clientCodec) ReadResponseBody(param interface{}) error {
	if param == nil {
		if c.response.ResponseLen != 0 {
			if err := read(c.r, make([]byte, c.response.ResponseLen)); err != nil {
				return err
			}
		}
		return nil
	}

	err := readResponseBody(c.r, &c.response, param)
	if err != nil {
		return nil
	}
	return nil
}

func readResponseHeader(r io.Reader, h *header.ResponseHeader) error {
	data, err := recvFrame(r)
	if err != nil {
		return err
	}
	return h.Unmarshal(data)
}

func writeRequest(w io.Writer, r *rpc.Request,
	compressType compressor.CompressType, param interface{}) error {
	if _, ok := compressor.Compressors[compressType]; !ok {
		return NotFoundCompressorError
	}
	reqBody, err := serializer.Serializers[serializer.Proto].Marshal(param)
	if err != nil {
		return err
	}
	compressedReqBody, err := compressor.Compressors[compressType].Zip(reqBody)
	if err != nil {
		return err
	}
	h := header.RequestPool.Get().(*header.RequestHeader)
	defer func() {
		h.ResetHeader()
		header.RequestPool.Put(h)
	}()
	h.ID = r.Seq
	h.Method = r.ServiceMethod
	h.RequestLen = uint32(len(compressedReqBody))
	h.CompressType = header.CompressType(compressType)
	h.Checksum = crc32.ChecksumIEEE(compressedReqBody)

	if err := sendFrame(w, h.Marshal()); err != nil {
		return err
	}
	if err := write(w, compressedReqBody); err != nil {
		return err
	}

	w.(*bufio.Writer).Flush()
	return nil
}

func readResponseBody(r io.Reader, h *header.ResponseHeader, param interface{}) error {
	respBody := make([]byte, h.ResponseLen)
	err := read(r, respBody)
	if err != nil {
		return err
	}

	if h.Checksum != 0 {
		if crc32.ChecksumIEEE(respBody) != h.Checksum {
			return UnexpectedChecksumError
		}
	}

	if _, ok := compressor.Compressors[compressor.CompressType(h.CompressType)]; !ok {
		return NotFoundCompressorError
	}

	resp, err := compressor.Compressors[compressor.CompressType(h.CompressType)].Unzip(respBody)
	if err != nil {
		return err
	}

	return serializer.Serializers[serializer.Proto].Unmarshal(resp, param)
}

func (c *clientCodec) Close() error {
	return c.c.Close()
}

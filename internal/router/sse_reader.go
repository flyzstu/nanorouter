package router

import (
	"bufio"
	"io"
	"sync"
)

const sseReaderSize = 4 << 10

var sseReaderPool = sync.Pool{New: func() any {
	return bufio.NewReaderSize(nil, sseReaderSize)
}}

func acquireSSEReader(source io.Reader) *bufio.Reader {
	reader := sseReaderPool.Get().(*bufio.Reader)
	reader.Reset(source)
	return reader
}

func releaseSSEReader(reader *bufio.Reader) {
	// 清除对响应体的引用，避免连接和缓冲区随池对象一起存活。
	reader.Reset(nil)
	sseReaderPool.Put(reader)
}

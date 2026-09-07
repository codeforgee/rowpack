package block

import (
	"bytes"
	"sync"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestZstdPooledDeterminism verifies pooled compress/decompress still round
// trips and stays deterministic.
func TestZstdPooledDeterminism(t *testing.T) {
	payload := bytes.Repeat([]byte("deterministic-block-payload-0123456789-abcdef"), 64)
	a, err := Compress(fileformat.CompressionZstd, 3, payload)
	require.NoError(t, err)
	b, err := Compress(fileformat.CompressionZstd, 3, payload)
	require.NoError(t, err)
	require.True(t, bytes.Equal(a, b), "zstd output not deterministic across pooled calls")
	out, err := Decompress(fileformat.CompressionZstd, nil, a, DefaultLimits().MaxRawBytes)
	require.NoError(t, err)
	require.True(t, bytes.Equal(out, payload), "round trip mismatch")
}

// TestZstdPoolConcurrent exercises pooled compress/decompress from many
// goroutines (the pools are shared and must be race-free).
func TestZstdPoolConcurrent(t *testing.T) {
	payloads := make([][]byte, 32)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte(i)}, 4096+i*13)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				p := payloads[(seed+i)%len(payloads)]
				c, err := Compress(fileformat.CompressionZstd, 3, p)
				if err != nil {
					assert.NoError(t, err)
					return
				}
				out, err := Decompress(fileformat.CompressionZstd, nil, c, DefaultLimits().MaxRawBytes)
				if err != nil {
					assert.NoError(t, err)
					return
				}
				if !bytes.Equal(out, p) {
					assert.Fail(t, "round trip mismatch")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

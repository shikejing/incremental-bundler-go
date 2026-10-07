package bundler

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// cacheFormatVersion 为缓存格式版本；版本不兼容时整体回退为空缓存（全量重建）。
const cacheFormatVersion = 1

// Cache 是内容寻址的持久化缓存。
//
// 布局：
//
//	<dir>/VERSION        格式版本号
//	<dir>/entries/<key>  缓存项，内容为 "<sha256(payload)>\n<payload>"
//
// 写入采用 临时文件+rename 的原子提交，任何时刻读到的都是完整项；
// 多进程共享时并发的 Put/Get 不会读到半写入状态。
type Cache struct {
	dir string
	mu  sync.RWMutex
	mem map[string][]byte
}

// OpenCache 打开或创建缓存目录。
// 版本文件缺失/损坏/不兼容时，安全清空并重建为空缓存，无需人工清理。
// dir 为空时返回纯内存缓存。
func OpenCache(dir string) (*Cache, error) {
	c := &Cache{dir: dir, mem: map[string][]byte{}}
	if dir == "" {
		return c, nil
	}
	entries := filepath.Join(dir, "entries")
	versionFile := filepath.Join(dir, "VERSION")
	data, err := os.ReadFile(versionFile)
	switch {
	case err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(cacheFormatVersion):
		// 版本兼容，直接复用。
	case os.IsNotExist(err):
		// 全新缓存目录：仅创建结构，不删除任何内容（多进程并发安全）。
		if err := os.MkdirAll(entries, 0o755); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(versionFile, []byte(strconv.Itoa(cacheFormatVersion)+"\n")); err != nil {
			return nil, err
		}
	default:
		// 损坏或不兼容：整体清除后重建，回退全量重建。
		if rmErr := os.RemoveAll(entries); rmErr != nil {
			return nil, fmt.Errorf("reset incompatible cache: %w", rmErr)
		}
		if err := os.MkdirAll(entries, 0o755); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(versionFile, []byte(strconv.Itoa(cacheFormatVersion)+"\n")); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// writeFileAtomic 写临时文件后 rename 提交；临时文件名唯一，并发写者互不干扰。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// encode 为缓存项编码：带自校验哈希，读取时可识别损坏。
func encode(payload []byte) []byte {
	sum := hashBytes(payload)
	buf := make([]byte, 0, len(sum)+1+len(payload))
	buf = append(buf, sum...)
	buf = append(buf, '\n')
	buf = append(buf, payload...)
	return buf
}

// decode 校验并解码缓存项；损坏时返回 ok=false。
func decode(data []byte) ([]byte, bool) {
	i := bytes.IndexByte(data, '\n')
	if i <= 0 {
		return nil, false
	}
	payload := data[i+1:]
	if hashBytes(payload) != string(data[:i]) {
		return nil, false
	}
	return payload, true
}

// Get 按缓存键读取；未命中或项损坏返回 ok=false（损坏项会被清除）。
func (c *Cache) Get(key string) ([]byte, bool, error) {
	c.mu.RLock()
	if v, ok := c.mem[key]; ok {
		c.mu.RUnlock()
		return v, true, nil
	}
	c.mu.RUnlock()
	if c.dir == "" {
		return nil, false, nil
	}
	path := filepath.Join(c.dir, "entries", key)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	payload, ok := decode(data)
	if !ok {
		// 损坏项：识别并清除，按未命中处理。
		_ = os.Remove(path)
		return nil, false, nil
	}
	c.mu.Lock()
	c.mem[key] = payload
	c.mu.Unlock()
	return payload, true, nil
}

// Put 原子写入缓存项：先写临时文件，fsync 后 rename 提交。
func (c *Cache) Put(key string, data []byte) error {
	c.mu.Lock()
	c.mem[key] = data
	c.mu.Unlock()
	if c.dir == "" {
		return nil
	}
	entries := filepath.Join(c.dir, "entries")
	if err := os.MkdirAll(entries, 0o755); err != nil {
		return err
	}
	final := filepath.Join(entries, key)
	tmp, err := os.CreateTemp(entries, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(encode(data)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// rename 是原子提交：并发读者要么看到旧值（不存在），要么看到完整新值。
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Close 目前所有写入均已即时持久化，保留接口以便演进。
func (c *Cache) Close() error { return nil }

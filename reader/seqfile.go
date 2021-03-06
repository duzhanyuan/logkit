package reader

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qiniu/log"
	"github.com/qiniu/logkit/utils"
)

// FileMode 读取单个文件模式
const FileMode = "file"

// DirMode 按时间顺序顺次读取文件夹下所有文件的模式
const DirMode = "dir"

const deafultFilePerm = 0600

// SeqFile 按最终修改时间依次读取文件的Reader类型
type SeqFile struct {
	meta *Meta

	dir              string   // 文件目录
	currFile         string   // 当前处理文件名
	f                *os.File // 当前处理文件
	inode            uint64   // 当前文件inode
	offset           int64    // 当前处理文件offset
	ignoreHidden     bool     // 忽略隐藏文件
	ignoreFileSuffix []string // 忽略文件后缀
	validFilePattern string   // 合法的文件名正则表达式
	stopped          int32    // 停止标志位

	lastSyncPath   string
	lastSyncOffset int64
}

func getStartFile(path, whence string, meta *Meta, sf *SeqFile) (f *os.File, dir, currFile string, offset int64, err error) {
	var pfi os.FileInfo
	dir, pfi, err = utils.GetRealPath(path)
	if err != nil || pfi == nil {
		log.Errorf("%s - utils.GetRealPath failed, err:%v", path, err)
		return
	}
	if !pfi.IsDir() {
		err = fmt.Errorf("%s -the path is not directory", dir)
		return
	}
	currFile, offset, err = meta.ReadOffset()
	if err != nil {
		switch whence {
		case WhenceOldest:
			currFile, offset, err = oldestFile(dir, sf.getIgnoreCondition())
		case WhenceNewest:
			currFile, offset, err = newestFile(dir, sf.getIgnoreCondition())
		default:
			err = errors.New("reader_whence paramter does not support: " + whence)
			return
		}
		if err != nil {
			if os.IsNotExist(err) {
				err = nil
				return
			}
			err = fmt.Errorf("%s -cannot open oldest file err:%v", dir, err)
			return
		}
	} else {
		log.Debugf("%v restore meta success", dir)
	}
	f, err = os.Open(currFile)
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
			return
		}
		err = fmt.Errorf("%s -cannot open currfile file err:%v", currFile, err)
		return
	}
	return
}

func NewSeqFile(meta *Meta, path string, ignoreHidden bool, suffixes []string, validFileRegex, whence string) (sf *SeqFile, err error) {
	sf = &SeqFile{
		ignoreFileSuffix: suffixes,
		ignoreHidden:     ignoreHidden,
		validFilePattern: validFileRegex,
	}
	//原来的for循环替换成单次执行，启动的时候出错就直接报错给用户即可，不需要等待重试。
	f, dir, currFile, offset, err := getStartFile(path, whence, meta, sf)
	if err != nil {
		return
	}
	if f != nil {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		_, err = f.Seek(offset, os.SEEK_SET)
		if err != nil {
			f.Close()
			return nil, err
		}
		sf.inode = utils.GetInode(fi)
		sf.f = f
		sf.offset = offset
	} else {
		sf.inode = 0
		sf.f = nil
		sf.offset = 0
	}
	sf.meta = meta
	sf.dir = dir
	sf.currFile = currFile
	return sf, nil
}

func (sf *SeqFile) getIgnoreCondition() func(os.FileInfo) bool {
	return func(fi os.FileInfo) bool {

		if sf.ignoreHidden {
			if strings.HasPrefix(fi.Name(), ".") {
				return false
			}
		}
		for _, s := range sf.ignoreFileSuffix {
			if strings.HasSuffix(fi.Name(), s) {
				return false
			}
		}
		match, err := filepath.Match(sf.validFilePattern, fi.Name())
		if err != nil {
			log.Errorf("when read dir %s, get not valid file pattern. Error->%v", sf.dir, err)
			return false
		}

		return match
	}
}

func newestFile(logdir string, condition func(os.FileInfo) bool) (currFile string, offset int64, err error) {
	fi, err := getMaxFile(logdir, condition, modTimeLater)
	if err != nil {
		return
	}
	currFile = filepath.Join(logdir, fi.Name())
	f, err := os.Open(currFile)
	if err != nil {
		return
	}
	offset, err = f.Seek(0, os.SEEK_END)
	if err != nil {
		return
	}
	return currFile, offset, nil

}

func oldestFile(logdir string, condition func(os.FileInfo) bool) (currFile string, offset int64, err error) {
	fi, err := getMinFile(logdir, condition, modTimeLater)
	if err != nil {
		return
	}
	return filepath.Join(logdir, fi.Name()), 0, err
}

func (sf *SeqFile) Name() string {
	return "SeqFile:" + sf.dir
}

func (sf *SeqFile) Source() string {
	return sf.dir
}

func (sf *SeqFile) Close() (err error) {
	atomic.AddInt32(&sf.stopped, 1)
	if sf.f == nil {
		return
	}
	return sf.f.Close()
}

func (sf *SeqFile) Read(p []byte) (n int, err error) {
	var nextFileRetry int
	n = 0
	for n < len(p) {
		var n1 int
		if sf.f == nil {
			if atomic.LoadInt32(&sf.stopped) > 0 {
				return 0, errors.New("reader " + sf.Name() + " has been exited")
			}
			err = sf.newOpen()
			if err != nil {
				log.Warnf("%v new open error %v, sleep 3s and retry", sf.dir, err)
				time.Sleep(3 * time.Second)
				continue
			}
		}
		n1, err = sf.f.Read(p[n:])
		sf.offset += int64(n1)
		n += n1
		if err != nil {
			if err != io.EOF {
				return n, err
			}
			fi, err1 := sf.nextFile()
			if os.IsNotExist(err1) {
				if nextFileRetry >= 3 {
					return n, io.EOF
				}
				// dir removed or file rotated
				log.Debugf("%s - nextFile: %v", sf.dir, err1)
				time.Sleep(WaitNoSuchFile)
				nextFileRetry++
				continue
			}
			if err1 != nil {
				log.Debugf("%s - nextFile(file exist) but: %v", sf.dir, err1)
				return n, err1
			}
			if fi != nil {
				log.Infof("%s - nextFile: %s", sf.dir, fi.Name())
				err2 := sf.open(fi)
				if err2 != nil {
					return n, err2
				}
				//已经获得了下一个文件，没有EOF
				err = nil
			} else {
				time.Sleep(time.Millisecond * 500)
				return 0, io.EOF
			}
		}
	}
	return
}

func (sf *SeqFile) nextFile() (fi os.FileInfo, err error) {
	currFi, err := os.Stat(sf.currFile)
	var condition func(os.FileInfo) bool
	if err != nil {
		if !os.IsNotExist(err) {
			// 日志读取错误
			log.Errorf("stat current file error %v, need retry", err)
			return
		}
		// 当前读取的文件已经被删除
		log.Warnf("stat current file error %v, start to find the oldest file", err)
		condition = sf.getIgnoreCondition()
	} else {
		newerThanCurrFile := func(f os.FileInfo) bool {
			return f.ModTime().Unix() > currFi.ModTime().Unix()
		}
		condition = andCondition(newerThanCurrFile, sf.getIgnoreCondition())
	}
	fi, err = getMinFile(sf.dir, condition, modTimeLater)
	if err != nil {
		log.Debugf("getMinFile error %v", err)
		return nil, err
	}
	if sf.isNewFile(fi) {
		return fi, nil
	}
	return nil, nil
}

func (sf *SeqFile) isNewFile(newFileInfo os.FileInfo) bool {
	if newFileInfo == nil {
		return false
	}
	newInode := utils.GetInode(newFileInfo)
	newName := newFileInfo.Name()
	newFsize := newFileInfo.Size()
	if newInode != 0 && sf.inode != 0 && newInode == sf.inode {
		return false
	}
	if newInode != sf.inode {
		log.Debugf("%s - newInode: %d, l.inode: %d", sf.dir, newInode, sf.inode)
		return true
	}
	if newFsize < sf.offset {
		log.Debugf("%s - newFsize: %d, l.offset: %d", sf.dir, newFsize, sf.offset)
		return true
	}
	fname := filepath.Base(sf.currFile)
	if newName != fname {
		log.Debugf("%s - newName: %d, l.fname: %d", sf.dir, newName, fname)
		return true
	}
	return false
}

func (sf *SeqFile) newOpen() (err error) {
	fi, err1 := sf.nextFile()
	if os.IsNotExist(err1) {
		return fmt.Errorf("did'n find any file in dir %s - nextFile: %v", sf.dir, err1)
	}
	if err1 != nil {
		return fmt.Errorf("read file in dir %s error - nextFile: %v", sf.dir, err1)
	}
	if fi == nil {
		return fmt.Errorf("nextfile info in dir %v is nil", sf.dir)
	}
	fname := fi.Name()
	sf.currFile = filepath.Join(sf.dir, fname)
	f, err := os.Open(sf.currFile)
	if os.IsNotExist(err) {
		return fmt.Errorf("os.Open %s: %v", fname, err)
	}
	if err != nil {
		return fmt.Errorf("os.Open %s: %v", fname, err)
	}
	sf.f = f
	sf.offset = 0
	sf.inode = utils.GetInode(fi)
	return
}

func (sf *SeqFile) open(fi os.FileInfo) (err error) {
	if fi == nil {
		return
	}
	err = sf.f.Close()
	if err != nil && err != syscall.EINVAL {
		log.Warnf("%s - %s f.Close: %v", sf.dir, sf.currFile)
		return
	}

	doneFile := sf.currFile
	fname := fi.Name()
	sf.currFile = filepath.Join(sf.dir, fname)
	for {
		f, err := os.Open(sf.currFile)
		if os.IsNotExist(err) {
			log.Debugf("os.Open %s: %v", fname, err)
			time.Sleep(WaitNoSuchFile)
			continue
		}
		if err != nil {
			log.Warnf("os.Open %s: %v", fname, err)
			return err
		}
		sf.f = f
		sf.offset = 0
		sf.inode = utils.GetInode(fi)
		log.Infof("%s - start tail new file: %s", sf.dir, fname)
		break
	}
	for {
		err = sf.meta.AppendDoneFile(doneFile)
		if err != nil {
			log.Errorf("cannot write done file %s, err:%v", doneFile, err)
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	return
}

func (sf *SeqFile) SyncMeta() (err error) {
	if sf.lastSyncOffset == sf.offset && sf.lastSyncPath == sf.currFile {
		log.Debugf("%v was just syncd %v %v ignore it...", sf.Name(), sf.lastSyncPath, sf.lastSyncOffset)
		return nil
	}
	sf.lastSyncOffset = sf.offset
	sf.lastSyncPath = sf.currFile
	return sf.meta.WriteOffset(sf.currFile, sf.offset)
}

package capwap

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/beego/beego/v2/core/logs"
)

// 计算配置文件的hash值
// ap/mr客户端直接调用的方法

var mutex sync.Mutex   // files更新锁
var prevTime time.Time // files上次更新时间
var totalHash string   // 所有配置文件的hash
var files MsgCfgFiles  // ap/mr设备的配置文件表, 每分钟更新一次[从/etc/config/目录下检索]

// 获取配置文件的总hash值, 检查配置文件变更周期是5分钟
func GetTotalHash() string {
	// 超时更新
	if time.Since(prevTime) > time.Minute*5 {
		// 遍历/etc/config目录下所有文件
		fs, err := os.ReadDir("/etc/config")
		if err != nil {
			logs.Error("read dir[/etc/config/] error:%s", err.Error())
			return totalHash
		}
		mutex.Lock()
		defer mutex.Unlock()
		files.Files = nil
		files.Files = make([]*CfgFile, 0, 100)
		hashBuilder := strings.Builder{}
		for _, f := range fs {
			if !f.IsDir() {
				file := CfgFile{}
				file.Name = f.Name()
				file.Content, err = os.ReadFile("/etc/config/" + file.Name)
				if err != nil {
					logs.Error("read config file[%s] error:%s", file.Name, err.Error())
					return totalHash
				}
				h := md5.New()
				bs := h.Sum(file.Content)
				file.CliHash = hex.EncodeToString(bs)
				files.Files = append(files.Files, &file)
				hashBuilder.WriteString(file.CliHash)
			}
		}
		h := md5.New()
		bs := h.Sum([]byte(hashBuilder.String()))
		totalHash = hex.EncodeToString(bs)
	}
	return totalHash
}

// 获取本机上所有的配置文件[/etc/config/*]
func GetFilesHash() []*CfgFile {
	mutex.Lock()
	defer mutex.Unlock()
	// copy是为了避免在返回后[释放锁],有GetTotalHash调用, 修改files信息
	fs := make([]*CfgFile, 0, len(files.Files))
	copy(fs, files.Files)
	return fs
}

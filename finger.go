package capwap

import (
	"os"
	"path/filepath"
)

// 计算管理文件的指纹信息

// 获取指定文件的指纹信息，文件修改的时间【时间戳】
func GetFinger(fileNameWithPath string) int64 {
	info, err := os.Stat(fileNameWithPath)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

// 获取指定路径下的所有文件【被管理的文件】指纹之和
func GetFingerAllFiles(path string) int64 {
	var sum int64 = 0
	for i := int32(0); i <= 32; i++ { // 有效值为protofile重定义的文件
		pfile := filepath.Join(path, CfgFilenameType(i).String())
		sum += GetFinger(pfile)
	}

	return sum
}

package svc

import (
	"eggdfs/common"
	"eggdfs/common/model"
	"eggdfs/logger"
	"eggdfs/svc/conf"
	"eggdfs/util"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"github.com/shirou/gopsutil/v3/disk"
	"go.uber.org/zap"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	storageDBFileName = "storage"
)

type Storage struct {
	db         *model.EggDB
	httpSchema string
	trackers   []string
}

type StorageStatus struct {
	Group      string `json:"group"`
	HttpSchema string `json:"http_schema"`
	Host       string `json:"host"`
	Port       string `json:"port"`
	Free       uint64 `json:"free"`
}

func NewStorage() *Storage {
	return &Storage{
		db:         model.NewEggDB(storageDBFileName),
		httpSchema: config().HttpSchema,
		trackers:   config().Storage.Trackers,
	}
}

func hello(c *gin.Context) {
	c.JSON(http.StatusOK, model.RespResult{
		Status:  common.Success,
		Message: "hello eggdfs storage!",
		Data:    nil,
	})
}

//QuickUpload 适合小文件
func (s *Storage) QuickUpload(c *gin.Context) {
	//用户自定义的存储文件夹
	fileHash := c.GetHeader(common.HeaderFileHash)
	logger.Info(fileHash)
	//秒传 检查数据库是否存在相同的md5
	if fileHash != "" {
		fi := model.FileInfo{}
		if exist, _ := s.db.IsExistKey(fileHash); exist {
			data, _ := s.db.Get(fileHash)
			_ = json.Unmarshal(data, &fi)
			c.JSON(http.StatusOK, model.RespResult{
				Status:  common.Success,
				Message: "文件已存在，秒传成功",
				Data:    fi,
			})
			return
		}
	}

	customDir := c.GetHeader(common.HeaderUploadFileDir)
	filePath := util.GenFilePath(customDir)
	baseDir := config().Storage.StorageDir + "/" + filePath
	if _, err := os.Stat(baseDir); err != nil {
		err := os.MkdirAll(baseDir, os.ModePerm)
		p, _ := filepath.Abs(config().Storage.StorageDir)
		if err != nil {
			logger.Error("文件保存路径创建失败", zap.String("file_baseDir", p))
			go s.TransErrorLogToTracker(common.DirCreateFail, "文件保存路径创建失败"+p)
			c.JSON(http.StatusOK, model.RespResult{
				Status:  common.DirCreateFail,
				Message: "文件保存路径创建失败",
				Data:    nil,
			})
			return
		}
		logger.Info("文件保存路径创建成功", zap.String("file_baseDir", p))
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusOK, model.RespResult{
			Status:  common.FormFileNotFound,
			Message: "未能索引上传文件",
			Data:    nil,
		})
		return
	}

	//文件大小限制
	if config().Storage.FileSizeLimit > 0 && file.Size > config().Storage.FileSizeLimit {
		logger.Warn("文件大小超过限制", zap.String("file", file.Filename), zap.Int64("size", file.Size))
		c.JSON(http.StatusOK, model.RespResult{
			Status:  common.FileSizeExceeded,
			Message: "文件大小超过限制",
			Data:    nil,
		})
		return
	}

	//保存文件
	//文件名由雪花算法的服务器生成
	uuid := c.GetHeader(common.HeaderFileUUID)
	fileName := util.GenFileName(uuid, file.Filename)
	fullPath := baseDir + "/" + fileName
	md5hash, err := s.SaveQuickUploadedFile(file, fullPath, fileHash)
	if err != nil {
		c.JSON(http.StatusOK, model.RespResult{
			Status:  common.FileSaveFail,
			Message: err.Error(),
		})
		return
	}
	fi := model.FileInfo{
		FileId: uuid,
		Name:   file.Filename,
		ReName: fileName,
		Url:    s.GenFileStaticUrl(filePath, fileName),
		Path:   fmt.Sprintf("%s/%s", filePath, fileName),
		Md5:    md5hash,
		Size:   file.Size,
		Group:  config().Storage.Group,
	}
	bytes, _ := json.Marshal(fi)
	_ = s.db.Put(fi.Md5, bytes)
	c.Writer.Header().Set(common.HeaderFileUploadRes, strconv.Itoa(common.Success))
	c.Writer.Header().Set(common.HeaderFileHash, fi.Md5)
	c.Writer.Header().Set(common.HeaderFilePath, filePath+"/"+fileName)
	c.JSON(http.StatusOK, model.RespResult{
		Status:  common.Success,
		Message: "文件保存成功",
		Data:    fi,
	})
}

//GenFileStaticUrl 生成文件url
func (s *Storage) GenFileStaticUrl(basePath, filename string) (url string) {
	c := config()
	p := basePath + "/" + filename
	//todo domain域名
	url = fmt.Sprintf("%s://%s/%s/%s", s.httpSchema, net.JoinHostPort(c.Host, c.Port), c.Storage.Group, p)
	return
}

//SaveQuickUploadedFile 保存快传文件
func (s *Storage) SaveQuickUploadedFile(file *multipart.FileHeader, dst string, hash string) (md5hash string, err error) {
	src, err := file.Open()
	if err != nil {
		return
	}
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer src.Close()
	defer out.Close()
	_, err = io.Copy(out, src)
	if err != nil {
		return
	}
	//检查文件完整性
	local, err := os.Open(dst)
	if err != nil {
		return
	}
	md5hash, _ = util.GenMD5(local)
	logger.Info("md5", zap.String("md5", md5hash))
	if hash != md5hash && hash != "" {
		go os.Remove(dst)
		err = errors.New("file is already damaged")
		return
	}
	return md5hash, nil
}

//Download 下载
func (s *Storage) Download(c *gin.Context) {
	filePath := c.Query("file")
	if filePath == "" {
		c.JSON(http.StatusOK, model.RespResult{
			Status: common.ParamBindFail,
		})
		return
	}
	fullPath := config().Storage.StorageDir + "/" + filePath
	if _, err := os.Stat(fullPath); err != nil {
		c.JSON(http.StatusOK, model.RespResult{
			Status:  common.Fail,
			Message: "no such file",
		})
		return
	}
	filename := c.GetHeader(common.HeaderDownloadFilename)
	if filename == "" {
		filename = path.Base(filePath)
	}
	//对下载的文件重命名
	c.Writer.Header().Add("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Writer.Header().Add("Content-Type", util.GetFileContentType(path.Ext(filePath)))
	c.File(fullPath)
}

//Status 向tracker回报状态
func (s *Storage) Status() {
	c := config()
	status := &StorageStatus{
		Group:      c.Storage.Group,
		HttpSchema: s.httpSchema,
		Host:       c.Host,
		Port:       c.Port,
	}
	if stat, err := disk.Usage(c.Storage.StorageDir); err != nil {
		status.Free = 0
	} else {
		status.Free = stat.Free
	}

	for _, url := range s.trackers {
		go func(url string) {
			logger.Info("report to tracker", zap.String("tracker", url), zap.String("host", c.Host))
			_, _ = util.HttpPost(url+"/status", status, nil, time.Second)
		}(url)
	}
}

//Sync 文件同步
func (s *Storage) Sync(c *gin.Context) {
	var sync model.SyncFileInfo
	var syncFunc SyncFunc
	_ = c.ShouldBindJSON(&sync)
	logger.Info("sync file info", zap.Any("info", sync))
	//add
	if sync.Action == common.SyncAdd {
		syncFunc = s.SyncFileAdd
	}
	//delete
	if sync.Action == common.SyncDelete {
		syncFunc = s.SyncFileDelete
	}

	if syncFunc != nil {
		syncFunc(sync, c)
	}
}

//SyncFunc 同步函数
type SyncFunc func(model.SyncFileInfo, *gin.Context)

//SyncFileAdd 文件新增同步函数
func (s *Storage) SyncFileAdd(sync model.SyncFileInfo, c *gin.Context) {
	base := config().Storage.StorageDir + "/" + sync.FilePath
	if _, err := os.Stat(base); err != nil {
		err := os.MkdirAll(base, os.ModePerm)
		if err != nil {
			go s.TransErrorLogToTracker(common.DirCreateFail, "文件保存路径创建失败"+base)
			c.JSON(http.StatusOK, model.RespResult{
				Status: common.DirCreateFail,
			})
			return
		}
	}

	//download file
	url := fmt.Sprintf("%s/%s/%s/%s", sync.Src, sync.Group, sync.FilePath, sync.FileName)
	logger.Info("sync-add:url", zap.String("url", url))
	resp, err := http.Get(url)
	if err != nil {
		c.JSON(http.StatusOK, model.RespResult{
			Status: common.Fail,
		})
		return
	}
	//if resp != nil {
	//	//检查校验和
	//	md5hash, _ := util.GenMD5(resp.Body)
	//	if md5hash != sync.FileHash && sync.FileHash != "" {
	//		//report todo
	//		return
	//	}
	//}
	fullPath := base + "/" + sync.FileName
	f, err := os.Create(fullPath)
	if err != nil {
		go s.TransErrorLogToTracker(common.DirCreateFail, "文件保存路径创建失败"+fullPath)
		c.JSON(http.StatusOK, model.RespResult{
			Status: common.DirCreateFail,
		})
		return
	}
	l, err := io.Copy(f, resp.Body)
	defer f.Close()
	if err != nil || l <= 0 {
		go s.TransErrorLogToTracker(common.FileSaveFail, "文件同步保存失败"+fullPath)
		c.JSON(http.StatusOK, model.RespResult{
			Status: common.Fail,
		})
		return
	}

	info, _ := os.Stat(fullPath)
	fi := model.FileInfo{
		FileId: sync.FileId,
		Name:   info.Name(),
		ReName: info.Name(),
		Url:    s.GenFileStaticUrl(sync.FilePath, sync.FileName),
		Size:   info.Size(),
		Path:   sync.FilePath,
		Md5:    sync.FileHash,
		Group:  sync.Group,
	}
	bytes, _ := json.Marshal(fi)
	_ = s.db.Put(sync.FileHash, bytes)
	c.JSON(http.StatusOK, model.RespResult{
		Status: common.Success,
	})
}

//SyncFileDelete 文件删除同步函数
func (s *Storage) SyncFileDelete(sync model.SyncFileInfo, c *gin.Context) {
	gf := config()
	//拼接路径
	fullPath := strings.Join([]string{gf.Storage.StorageDir, sync.FilePath, sync.FileName}, "/")
	if _, err := os.Stat(fullPath); err != nil {
		c.JSON(http.StatusOK, model.RespResult{Status: common.Success})
		return
	}
	//删除文件
	err := os.Remove(fullPath)
	if err != nil {
		c.JSON(http.StatusOK, model.RespResult{Status: common.Fail})
		return
	}
	if sync.FileHash != "" {
		_ = s.db.Delete(sync.FileHash)
	}
	c.JSON(http.StatusOK, model.RespResult{Status: common.Success})
}

//TransErrorLogToTracker 同步错误日志到tracker
func (s *Storage) TransErrorLogToTracker(code int, msg string) {
	type errMsg struct {
		ErrCode int    `json:"err_code"`
		Group   string `json:"group"`
		Host    string `json:"host"`
		Port    string `json:"port"`
		ErrMsg  string `json:"err_msg"`
	}
	c := config()
	for _, url := range s.trackers {
		m := errMsg{
			ErrCode: code,
			Group:   c.Storage.Group,
			Host:    c.Host,
			Port:    c.Port,
			ErrMsg:  msg,
		}
		util.HttpPost(url+"/err/log", m, nil, time.Second*5)
	}
}

//startTimerTask 启动定时任务
func (s *Storage) startTimerTask() error {
	cr := cron.New(cron.WithSeconds())
	//5s per
	_, err := cr.AddFunc("*/5 * * * * *", func() {
		s.Status()
	})
	if err != nil {
		return err
	}
	cr.Start()
	return nil
}

//Start 启动Storage服务
func (s *Storage) Start() {
	sd := config().Storage.StorageDir
	if _, err := os.Stat(sd); err != nil {
		err := os.MkdirAll(sd, os.ModePerm)
		p, _ := filepath.Abs(config().Storage.StorageDir)
		if err != nil {
			go s.TransErrorLogToTracker(common.DirCreateFail, "root文件夹创建失败"+p)
			logger.Error("文件保存路径创建失败", zap.String("storage_dir", p))
		}
		logger.Info("文件保存路径创建成功", zap.String("storage_dir", p))
	}

	r := gin.Default()

	//file system
	r.StaticFS(conf.Config().Storage.Group, http.Dir(config().Storage.StorageDir))

	r.GET("/hello", hello)

	//download file
	r.GET("/download", s.Download)

	//sync file
	r.POST("/sync", s.Sync)
	r.Group("/v1")
	{
		//upload file
		r.POST("/upload", s.QuickUpload)
	}

	//开启定时任务
	if err := s.startTimerTask(); err != nil {
		logger.Panic("Storage定时任务启动失败", zap.String("addr", config().Host))
	}

	err := r.Run(":" + config().Port)
	if err != nil {
		logger.Panic("Storage服务启动失败", zap.String("addr", config().Host))
	}
}

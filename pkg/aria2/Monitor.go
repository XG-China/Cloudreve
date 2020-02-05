package aria2

import (
	"context"
	"encoding/json"
	"errors"
	model "github.com/HFO4/cloudreve/models"
	"github.com/HFO4/cloudreve/pkg/filesystem"
	"github.com/HFO4/cloudreve/pkg/filesystem/driver/local"
	"github.com/HFO4/cloudreve/pkg/filesystem/fsctx"
	"github.com/HFO4/cloudreve/pkg/task"
	"github.com/HFO4/cloudreve/pkg/util"
	"github.com/zyxar/argo/rpc"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

// Monitor 离线下载状态监控
type Monitor struct {
	Task     *model.Download
	Interval time.Duration

	notifier chan StatusEvent
}

// StatusEvent 状态改变事件
type StatusEvent struct {
	GID    string
	Status int
}

// NewMonitor 新建上传状态监控
func NewMonitor(task *model.Download) {
	monitor := &Monitor{
		Task:     task,
		Interval: time.Duration(model.GetIntSetting("aria2_interval", 10)) * time.Second,
		notifier: make(chan StatusEvent),
	}
	go monitor.Loop()
	EventNotifier.Subscribe(monitor.notifier, monitor.Task.GID)
}

// Loop 开启监控循环
func (monitor *Monitor) Loop() {
	defer EventNotifier.Unsubscribe(monitor.Task.GID)

	// 首次循环立即更新
	interval := time.Duration(0)

	for {
		select {
		case <-monitor.notifier:
			if monitor.Update() {
				return
			}
		case <-time.After(interval):
			interval = monitor.Interval
			if monitor.Update() {
				return
			}
		}
	}
}

// Update 更新状态，返回值表示是否退出监控
func (monitor *Monitor) Update() bool {
	status, err := Instance.Status(monitor.Task)
	if err != nil {
		util.Log().Warning("无法获取下载任务[%s]的状态，%s", monitor.Task.GID, err)
		monitor.setErrorStatus(err)
		monitor.RemoveTempFolder()
		return true
	}

	// 磁力链下载需要跟随
	if len(status.FollowedBy) > 0 {
		util.Log().Debug("离线下载[%s]重定向至[%s]", monitor.Task.GID, status.FollowedBy[0])
		monitor.Task.GID = status.FollowedBy[0]
		monitor.Task.Save()
		return false
	}

	// 更新任务信息
	if err := monitor.UpdateTaskInfo(status); err != nil {
		util.Log().Warning("无法更新下载任务[%s]的任务信息[%s]，", monitor.Task.GID, err)
		monitor.setErrorStatus(err)
		return true
	}

	util.Log().Debug(status.Status)

	switch status.Status {
	case "complete":
		return monitor.Complete(status)
	case "error":
		return monitor.Error(status)
	case "active", "waiting", "paused":
		return false
	case "removed":
		return true
	default:
		util.Log().Warning("下载任务[%s]返回未知状态信息[%s]，", monitor.Task.GID, status.Status)
		return true
	}
}

// UpdateTaskInfo 更新数据库中的任务信息
func (monitor *Monitor) UpdateTaskInfo(status rpc.StatusInfo) error {
	originSize := monitor.Task.TotalSize
	originPath := monitor.Task.Path

	monitor.Task.GID = status.Gid
	monitor.Task.Status = getStatus(status.Status)

	// 文件大小、已下载大小
	total, err := strconv.ParseUint(status.TotalLength, 10, 64)
	if err != nil {
		total = 0
	}
	downloaded, err := strconv.ParseUint(status.CompletedLength, 10, 64)
	if err != nil {
		downloaded = 0
	}
	monitor.Task.TotalSize = total
	monitor.Task.DownloadedSize = downloaded
	monitor.Task.GID = status.Gid
	monitor.Task.Parent = status.Dir

	// 下载速度
	speed, err := strconv.Atoi(status.DownloadSpeed)
	if err != nil {
		speed = 0
	}

	monitor.Task.Speed = speed
	if len(status.Files) > 0 {
		monitor.Task.Path = status.Files[0].Path
	}
	attrs, _ := json.Marshal(status)
	monitor.Task.Attrs = string(attrs)

	if err := monitor.Task.Save(); err != nil {
		return nil
	}

	if originSize != monitor.Task.TotalSize || originPath != monitor.Task.Path {
		// 大小、文件名更新后，对文件限制等进行校验
		if err := monitor.ValidateFile(); err != nil {
			// 验证失败时取消任务
			monitor.Cancel()
			return err
		}
	}

	return nil
}

// Cancel 取消上传并尝试删除临时文件
func (monitor *Monitor) Cancel() {
	if err := Instance.Cancel(monitor.Task); err != nil {
		util.Log().Warning("无法取消离线下载任务[%s], %s", monitor.Task.GID, err)
	}
	util.Log().Debug("离线下载任务[%s]已取消，1 分钟后删除临时文件", monitor.Task.GID)
	go func(monitor *Monitor) {
		select {
		case <-time.After(time.Duration(60) * time.Second):
			monitor.RemoveTempFolder()
		}
	}(monitor)
}

// ValidateFile 上传过程中校验文件大小、文件名
func (monitor *Monitor) ValidateFile() error {
	// 找到任务创建者
	user := monitor.Task.GetOwner()
	if user == nil {
		return ErrUserNotFound
	}

	// 创建文件系统
	fs, err := filesystem.NewFileSystem(user)
	if err != nil {
		return err
	}
	defer fs.Recycle()

	// 创建上下文环境
	ctx := context.WithValue(context.Background(), fsctx.FileHeaderCtx, local.FileStream{
		Size: monitor.Task.TotalSize,
		Name: filepath.Base(monitor.Task.Path),
	})

	// 验证文件
	if err := filesystem.HookValidateFile(ctx, fs); err != nil {
		return err
	}

	// 验证用户容量
	if err := filesystem.HookValidateCapacityWithoutIncrease(ctx, fs); err != nil {
		return err
	}

	return nil
}

// Error 任务下载出错处理，返回是否中断监控
func (monitor *Monitor) Error(status rpc.StatusInfo) bool {
	monitor.setErrorStatus(errors.New(status.ErrorMessage))

	// 清理临时文件
	monitor.RemoveTempFolder()

	return true
}

// RemoveTempFile 清理下载临时文件
func (monitor *Monitor) RemoveTempFile() {
	err := os.Remove(monitor.Task.Path)
	if err != nil {
		util.Log().Warning("无法删除离线下载临时文件[%s], %s", monitor.Task.Path, err)
	}

	if empty, _ := util.IsEmpty(monitor.Task.Parent); empty {
		err := os.Remove(monitor.Task.Parent)
		if err != nil {
			util.Log().Warning("无法删除离线下载临时目录[%s], %s", monitor.Task.Parent, err)
		}
	}
}

// RemoveTempFolder 清理下载临时目录
func (monitor *Monitor) RemoveTempFolder() {
	err := os.RemoveAll(monitor.Task.Parent)
	if err != nil {
		util.Log().Warning("无法删除离线下载临时目录[%s], %s", monitor.Task.Parent, err)
	}

}

// Complete 完成下载，返回是否中断监控
func (monitor *Monitor) Complete(status rpc.StatusInfo) bool {
	// 创建中转任务
	job, err := task.NewTransferTask(
		monitor.Task.UserID,
		path.Join(monitor.Task.Dst, filepath.Base(monitor.Task.Path)),
		monitor.Task.Path,
		monitor.Task.Parent,
	)
	if err != nil {
		monitor.setErrorStatus(err)
		return true
	}

	// 提交中转任务
	task.TaskPoll.Submit(job)

	// 更新任务ID
	monitor.Task.TaskID = job.Model().ID
	monitor.Task.Save()

	return true
}

func (monitor *Monitor) setErrorStatus(err error) {
	monitor.Task.Status = Error
	monitor.Task.Error = err.Error()
	monitor.Task.Save()
}

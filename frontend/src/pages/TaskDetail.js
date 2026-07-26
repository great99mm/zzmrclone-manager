import React, { useState, useEffect, useRef, useCallback } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import {
  ArrowLeft,
  Play,
  Square,
  Pause,
  Ban,
  RotateCcw,
  Pencil,
  Trash2,
  Terminal,
  Activity,
  Clock,
  CheckCircle2,
  Upload,
  File,
  ShieldCheck,
  ListChecks,
  Database,
  ChevronDown,
  Network
} from 'lucide-react';
import { getTask, getTaskStatus, getProactiveStatus, resolveProactiveBatch, manualResetQuotaAccount, startTask, stopTask, pauseTask, cancelTask, dedupeTask, startProactiveManualMerge, closeProactiveUnknownMaintenance, deleteTask } from '../services/api';
import { createWebSocket } from '../services/api';
import toast from 'react-hot-toast';
import { QuotaAccountBar, QuotaExhaustedNotice } from '../components/QuotaAccountBar';

const parseRotationRemotes = (value) => {
  try {
    const parsed = JSON.parse(value || '[]');
    return Array.isArray(parsed) ? parsed.filter(Boolean) : [];
  } catch {
    return (value || '').split(',').map(item => item.trim()).filter(Boolean);
  }
};

const formatTaskDest = (task) => {
  if (task.dest_type === 'local') return `📂 ${task.remote_dir || ''}`;
  if (task.task_type === 'rotation') {
    const remotes = parseRotationRemotes(task.rotation_remotes);
    return `☁ ${(remotes.length ? remotes.join(' / ') : task.remote_name || '')}:${task.remote_dir || ''}`;
  }
  return `☁ ${task.remote_name || ''}:${task.remote_dir || ''}`;
};

const TaskDetail = () => {
  const { id } = useParams();
  const navigate = useNavigate();
  const [task, setTask] = useState(null);
  const [status, setStatus] = useState({ status: 'idle', running: false });
  const [qbStatus, setQbStatus] = useState(null);
  const [proactiveStatus, setProactiveStatus] = useState(null);
  const [proactiveStatusLoading, setProactiveStatusLoading] = useState(false);
  const [proactiveStatusError, setProactiveStatusError] = useState('');
  const [resolutionState, setResolutionState] = useState({ batchId: null, action: '', loading: false, error: '', success: '' });
  const [legacyRecoveryState, setLegacyRecoveryState] = useState({ loading: false, error: '' });
  const [loading, setLoading] = useState(true);
  const [fileProgresses, setFileProgresses] = useState({});
	const [pauseMenuOpen, setPauseMenuOpen] = useState(false);
  const wsRef = useRef(null);
  const progressTimerRef = useRef(null);

  // 从单条日志解析 transferring 进度
  const parseLogProgress = useCallback((line) => {
    // 匹配 rclone stats transferring 行，如：
    // * test_file_1.dat:  6% /10Gi, 16.075Mi/s, 9m55s
    // test_file_1.dat:  6% /10Gi, 16.075Mi/s, 9m55s
    const match = line.match(/^\s*(?:\*\s*)?(.+?):\s+(\d+(?:\.\d+)?%)\s+\/([^,]+)(?:,\s*([^,]+))?/);
    if (match) {
      const fileName = match[1].trim();
      const percent = parseFloat(match[2]);
      const sizeStr = match[3].trim();
      const speedStr = (match[4] || '').trim();
      setFileProgresses(prev => ({
        ...prev,
        [fileName]: {
          progress: percent,
          sizeStr,
          speedStr,
          lastUpdate: Date.now(),
        }
      }));
    }
  }, []);

  // 清理超过30秒未更新的文件进度（视为已完成）
  // 30秒超时兜底，即使偶尔丢日志进度条也不会消失
  const cleanupStaleProgresses = useCallback(() => {
    setFileProgresses(prev => {
      const now = Date.now();
      const updated = {};
      let changed = false;
      for (const [name, data] of Object.entries(prev)) {
        if (now - data.lastUpdate < 30000) {
          updated[name] = data;
        } else {
          changed = true;
        }
      }
      return changed ? updated : prev;
    });
  }, []);

  const loadTask = useCallback(async () => {
    try {
      const res = await getTask(id);
      setTask(res.data);
    } catch (err) {
      toast.error('加载任务失败');
      navigate('/tasks');
    } finally {
      setLoading(false);
    }
  }, [id, navigate]);

  const loadStatus = useCallback(async () => {
    try {
      const res = await getTaskStatus(id);
      setStatus(res.data);
      setQbStatus(res.data.qb_status || null);
      setTask(prev => prev ? {
        ...prev,
        remote_name: res.data.remote_name ?? prev.remote_name,
        rotation_current_index: res.data.rotation_current_index ?? prev.rotation_current_index,
        rotation_current_round: res.data.rotation_current_round ?? prev.rotation_current_round,
        rotation_paused_until: res.data.rotation_paused_until ?? prev.rotation_paused_until,
      } : prev);
    } catch (err) {
      console.error('Failed to load status');
    }
  }, [id]);

  const loadProactiveStatus = useCallback(async () => {
    setProactiveStatusLoading(true);
    setProactiveStatusError('');
    try {
      const res = await getProactiveStatus(id);
      setProactiveStatus(res.data);
      return true;
    } catch (err) {
      setProactiveStatusError(err.response?.data?.error || '主动额度状态暂时无法获取');
      return false;
    } finally {
      setProactiveStatusLoading(false);
    }
  }, [id]);

  const handleResolveBatch = useCallback(async (batchId, action, resolution) => {
    const actionText = action === 'accept_moved' ? '确认本地文件已由 rclone 移动完成' : '恢复本地文件并释放额度';
    if (!window.confirm(`${actionText}？此操作会改变批次状态。`)) return;
    setResolutionState({ batchId, fileId: resolution.fileId, action, loading: true, error: '', success: '' });
    try {
      await resolveProactiveBatch(id, {
        batch_id: batchId,
        file_id: resolution.fileId,
        action,
        expected_state: resolution.expectedState,
        expected_updated_at: resolution.expectedUpdatedAt,
      });
      setResolutionState({ batchId, fileId: resolution.fileId, action, loading: false, error: '', success: '处理已提交，状态正在刷新。' });
      await loadProactiveStatus();
      await loadStatus();
    } catch (err) {
      if (err.response?.status === 409) {
        await loadProactiveStatus();
        await loadStatus();
        setResolutionState({ batchId, fileId: resolution.fileId, action, loading: false, error: '批次状态已变化，已刷新最新状态。请重新查看该批次后再处理。', success: '' });
      } else {
        setResolutionState({ batchId, fileId: resolution.fileId, action, loading: false, error: err.response?.data?.error || '处理失败，请稍后重试。', success: '' });
      }
    }
  }, [id, loadProactiveStatus, loadStatus]);

  const handleManualReset = useCallback(async (accountId) => {
    await manualResetQuotaAccount(id, accountId);
    await loadProactiveStatus();
  }, [id, loadProactiveStatus]);

  useEffect(() => {
    loadTask();
    loadStatus();

    const ws = createWebSocket();
    wsRef.current = ws;

    ws.onmessage = (event) => {
      const data = JSON.parse(event.data);
      if (data.task_id === parseInt(id)) {
        if (data.type === 'log') {
          // 无论是否显示日志，都解析 transferring 进度（保证上传部分正常工作）
          parseLogProgress(data.content);

        } else if (data.type === 'task_complete') {
          toast.success('任务执行完成');
          setFileProgresses({});
          loadStatus();
        } else if (data.type === 'task_error') {
          toast.error(`任务异常: ${data.error}`);
          setFileProgresses({});
          loadStatus();
        } else if (data.type === 'task_started') {
          loadStatus();
        } else if (data.type === 'task_stopped') {
          loadStatus();
        } else if (data.type === 'file_progress') {
          // 兼容后端 WebSocket file_progress 消息
          setFileProgresses(prev => ({
            ...prev,
            [data.file_name]: {
              progress: data.progress || 0,
              bytes: data.bytes || 0,
              size: data.size || 0,
              speed: data.speed || 0,
              sizeStr: formatBytes(data.size || 0),
              speedStr: formatSpeed(data.speed || 0),
              lastUpdate: Date.now(),
            }
          }));
        }
      }
    };

    // Poll status every 2 seconds (was 3s)
    const interval = setInterval(() => {
      loadStatus();
    }, 2000);

    // Cleanup stale progresses every 2 seconds
    progressTimerRef.current = setInterval(cleanupStaleProgresses, 2000);

    return () => {
      ws.close();
      clearInterval(interval);
      if (progressTimerRef.current) clearInterval(progressTimerRef.current);
    };
  }, [id, cleanupStaleProgresses, parseLogProgress, loadTask, loadStatus]);

  useEffect(() => {
    if (!task || task.task_type !== 'rotation' || task.rotation_strategy !== 'proactive_quota') return undefined;
    let active = true;
    let timer;
    const poll = async () => {
      const succeeded = await loadProactiveStatus();
      if (active) timer = setTimeout(poll, succeeded ? 2000 : 10000);
    };
    poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [task?.task_type, task?.rotation_strategy, loadProactiveStatus]);

  const handleStart = async () => {
    try {
      await startTask(id);
      toast.success('任务已启动');
      // Small delay so the backend has time to update DB + IsRunning state.
      setTimeout(loadStatus, 300);
    } catch (err) {
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async () => {
    try {
      await stopTask(id);
      toast.success('任务已停止');
      setFileProgresses({});
      setTimeout(loadStatus, 300);
    } catch (err) {
      toast.error('停止失败');
    }
  };

  const handlePause = async (mode) => {
    try {
      await pauseTask(id, mode);
      toast.success(mode === 'after_current' ? '当前批次完成后将暂停' : '任务已立即暂停');
      setPauseMenuOpen(false);
      setFileProgresses({});
      setTimeout(loadStatus, 300);
      setTimeout(loadTask, 300);
    } catch (err) {
      toast.error(err.response?.data?.error || '暂停失败');
    }
  };

  const handleCancel = async () => {
    try {
      await cancelTask(id);
      toast.success('任务已停止');
      setFileProgresses({});
      setTimeout(loadStatus, 300);
      setTimeout(loadTask, 300);
    } catch (err) {
      toast.error(err.response?.data?.error || '停止失败');
    }
  };

  const handleDedupe = async () => {
    try {
      await dedupeTask(id);
      toast.success('去重任务已启动');
    } catch (err) {
      toast.error('去重失败');
    }
  };

  const handleProactiveManualMerge = async () => {
    if (!window.confirm('确定开始合并吗？这会在目标位置执行去重，可能改变远端文件。')) return;
    try {
      await startProactiveManualMerge(id);
      toast.success('合并已提交，状态正在刷新。');
      await loadProactiveStatus();
    } catch (err) {
      if (err.response?.status === 409) {
        await loadProactiveStatus();
        toast.error('当前无法开始合并，已刷新最新状态。');
      } else {
        toast.error(err.response?.data?.error || '开始合并失败');
      }
    }
  };

  const handleLegacyRecovery = async (recovery) => {
    if (!recovery?.epoch_id || recovery.dedupe_state !== 'unknown' || !recovery.process_identity_available) return;
    if (!window.confirm('确认相关处理已经停止，并关闭这条待确认状态吗？')) return;
    setLegacyRecoveryState({ loading: true, error: '' });
    try {
      await closeProactiveUnknownMaintenance(recovery.epoch_id, {
        reason: recovery.reason,
        expected_state: 'unknown',
        expected_revision: recovery.revision,
      });
      toast.success('恢复已完成，状态正在刷新。');
      await loadProactiveStatus();
    } catch (err) {
      if (err.response?.status === 409) {
        await loadProactiveStatus();
        toast.error('状态已变化，已刷新最新状态。');
      } else {
        const message = '恢复失败，请稍后重试。';
        setLegacyRecoveryState({ loading: false, error: message });
        toast.error(message);
        return;
      }
    }
    setLegacyRecoveryState({ loading: false, error: '' });
  };

  const handleDelete = async () => {
    if (!window.confirm('确定要删除这个任务吗？此操作不可恢复。')) return;
    try {
      await deleteTask(id);
      toast.success('任务已删除');
      navigate('/tasks');
    } catch (err) {
      toast.error('删除失败');
    }
  };

  if (loading || !task) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  const isQuickTask = !!task.is_quick_task;
  const isProactiveQuotaTask = task.task_type === 'rotation' && task.rotation_strategy === 'proactive_quota';
  const canContinueQuickTask = isQuickTask && (status.status === 'paused' || status.status === 'error');
  const rotationRemotes = parseRotationRemotes(task.rotation_remotes);
  const rotationCurrentRemote = rotationRemotes[task.rotation_current_index || 0] || rotationRemotes[0] || '-';

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
        <div className="flex items-center gap-4">
          <button
            onClick={() => navigate(isQuickTask ? '/files' : '/tasks')}
            className="p-2 hover:bg-gray-100 rounded-lg transition-colors"
          >
            <ArrowLeft className="w-5 h-5" />
          </button>
          <div>
            <div className="flex items-center gap-2">
              <h1 className="text-2xl font-bold text-gray-900">{task.name}</h1>
              <StatusBadge status={status.status} />
            </div>
            <p className="text-gray-500 mt-1">
              {(task.source_type === 'remote' ? '☁ ' : '📂 ') + task.source_dir}
              {' → '}
              {formatTaskDest(task)}
            </p>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-1.5 md:gap-2">
          {isQuickTask ? (
            <>
              {status.running ? (
                <>
                  <button
                    onClick={handlePause}
                    className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-amber-50 text-amber-600 rounded-lg hover:bg-amber-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <Pause className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">暂停</span>
                  </button>
                  <button
                    onClick={handleCancel}
                    className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-red-50 text-red-600 rounded-lg hover:bg-red-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <Ban className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">停止</span>
                  </button>
                </>
              ) : canContinueQuickTask ? (
                <>
                  <button
                    onClick={handleStart}
                    className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-green-50 text-green-600 rounded-lg hover:bg-green-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <Play className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">继续</span>
                  </button>
                  <button
                    onClick={handleCancel}
                    className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-red-50 text-red-600 rounded-lg hover:bg-red-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <Ban className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">停止</span>
                  </button>
                </>
              ) : null}
            </>
          ) : (
            <>
              {status.running ? (
                isProactiveQuotaTask ? (
                  <div className="relative inline-flex">
                    <button
                      onClick={() => handlePause('after_current')}
                      className="inline-flex items-center gap-1 px-3 md:px-4 py-2 bg-amber-50 text-amber-700 rounded-l-lg hover:bg-amber-100 transition-colors font-medium text-sm md:text-base"
                    >
                      <Pause className="w-3.5 h-3.5 md:w-4 md:h-4" />
                      <span className="hidden xs:inline">暂停</span>
                    </button>
                    <button
                      type="button"
                      aria-label="选择暂停方式"
                      aria-expanded={pauseMenuOpen}
                      onClick={() => setPauseMenuOpen(open => !open)}
                      className="inline-flex items-center justify-center w-9 border-l border-amber-200 bg-amber-50 text-amber-700 rounded-r-lg hover:bg-amber-100 transition-colors"
                    >
                      <ChevronDown className="w-4 h-4" />
                    </button>
                    {pauseMenuOpen && (
                      <div className="absolute right-0 top-full z-20 mt-1 w-44 border border-gray-200 bg-white shadow-lg rounded-lg py-1">
                        <button type="button" onClick={() => handlePause('after_current')} className="w-full px-3 py-2 text-left text-sm text-gray-700 hover:bg-gray-50">完成当前批次后暂停</button>
                        <button type="button" onClick={() => handlePause('immediate')} className="w-full px-3 py-2 text-left text-sm text-red-700 hover:bg-red-50">立刻暂停</button>
                      </div>
                    )}
                  </div>
                ) : (
                  <button
                    onClick={handleStop}
                    className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-red-50 text-red-600 rounded-lg hover:bg-red-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <Square className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">停止</span>
                  </button>
                )
              ) : (
                <button
                  onClick={handleStart}
                  className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-green-50 text-green-600 rounded-lg hover:bg-green-100 transition-colors font-medium text-sm md:text-base"
                >
                  <Play className="w-3.5 h-3.5 md:w-4 md:h-4" />
                  <span className="hidden xs:inline">启动</span>
                </button>
              )}
              {isProactiveQuotaTask ? (
                proactiveStatus?.maintenance?.manual_merge_available && (
                  <button
                    onClick={handleProactiveManualMerge}
                    aria-label="开始合并"
                    title="开始合并"
                    className="inline-flex items-center justify-center min-h-11 min-w-11 xs:min-h-0 xs:min-w-0 gap-1 md:gap-2 px-3 md:px-4 py-2 bg-purple-50 text-purple-600 rounded-lg hover:bg-purple-100 transition-colors font-medium text-sm md:text-base"
                  >
                    <RotateCcw className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    <span className="hidden xs:inline">开始合并</span>
                  </button>
                )
              ) : (
                <button
                  onClick={handleDedupe}
                  className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-purple-50 text-purple-600 rounded-lg hover:bg-purple-100 transition-colors font-medium text-sm md:text-base"
                >
                  <RotateCcw className="w-3.5 h-3.5 md:w-4 md:h-4" />
                  <span className="hidden xs:inline">去重</span>
                </button>
              )}
              <Link
                to={`/tasks/${id}/edit`}
                className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-blue-50 text-blue-600 rounded-lg hover:bg-blue-100 transition-colors font-medium text-sm md:text-base"
              >
                <Pencil className="w-3.5 h-3.5 md:w-4 md:h-4" />
                <span className="hidden xs:inline">编辑</span>
              </Link>
            </>
          )}
          <button
            onClick={handleDelete}
            className="inline-flex items-center gap-1 md:gap-2 px-3 md:px-4 py-2 bg-gray-50 text-gray-600 rounded-lg hover:bg-red-50 hover:text-red-600 transition-colors font-medium text-sm md:text-base"
          >
            <Trash2 className="w-3.5 h-3.5 md:w-4 md:h-4" />
            <span className="hidden xs:inline">删除</span>
          </button>
        </div>
      </div>

      {/* Info Cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        <InfoCard
          icon={Activity}
          label="并发传输"
          value={task.transfers}
          sub={`检查: ${task.checkers}`}
        />
        <InfoCard
          icon={Clock}
          label="最小年龄"
          value={task.min_age}
          sub={`重试: ${task.retries}次`}
        />
        <InfoCard
          icon={Terminal}
          label="块大小"
          value={task.drive_chunk_size}
          sub={`缓冲: ${task.buffer_size}`}
        />
        <InfoCard
          icon={CheckCircle2}
          label="自动化"
          value={task.qb_enabled && !isProactiveQuotaTask ? 'qB完成触发' : (task.watch_enabled ? '监控' : '手动')}
          sub={task.qb_enabled && !isProactiveQuotaTask ? `轮询 ${task.qb_poll_interval || 60}秒` : (task.schedule_enabled ? `定时 ${task.schedule_interval}分` : '无定时')}
        />
        {task.qb_enabled && !isProactiveQuotaTask && (
          <InfoCard
            icon={CheckCircle2}
            label="qBittorrent"
            value={task.qb_url || '-'}
            sub={task.qb_delete_files ? '单种子转移后删种并删除文件' : '单种子转移后只删除种子'}
          />
        )}
        {task.task_type === 'rotation' && (
          <>
            <InfoCard
              icon={Upload}
              label="轮转网盘"
              value={rotationRemotes.join(' / ') || '-'}
              sub={`目标目录: ${task.remote_dir || '/'}`}
            />
            <InfoCard
              icon={RotateCcw}
              label="当前轮转"
              value={`第 ${(task.rotation_current_round || 0) + 1} 轮 / ${rotationCurrentRemote}`}
              sub={`当前账号序号: ${(task.rotation_current_index || 0) + 1}`}
            />
            <InfoCard
              icon={Clock}
              label="恢复信息"
              sub={`暂停至: ${task.rotation_paused_until || '未暂停'}`}
            />
          </>
        )}
      </div>

      {task.qb_enabled && !isProactiveQuotaTask && <QBQueuePanel status={qbStatus} />}

      {task.task_type === 'rotation' && task.rotation_strategy === 'proactive_quota' && (
        <ProactiveQuotaPanel
          status={proactiveStatus}
          loading={proactiveStatusLoading}
          error={proactiveStatusError}
          onRetry={loadProactiveStatus}
          taskId={id}
          onManualReset={handleManualReset}
          resolutionState={resolutionState}
          onResolve={handleResolveBatch}
          legacyRecoveryState={legacyRecoveryState}
          onRecover={handleLegacyRecovery}
        />
      )}

      {/* Active File Transfers */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
        <div className="px-6 py-4 border-b border-gray-100 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <Upload className="w-5 h-5 text-blue-500" />
            <h2 className="font-semibold text-gray-900">正在传输的文件</h2>
            {Object.keys(fileProgresses).length > 0 && (
              <span className="px-2 py-0.5 bg-blue-100 text-blue-700 text-xs font-medium rounded-full animate-pulse">
                {Object.keys(fileProgresses).length} 个文件
              </span>
            )}
          </div>
        </div>
        <div className="p-4 md:p-6 space-y-4">
          {isProactiveQuotaTask && proactiveStatus ? (
            (() => {
              const running = (proactiveStatus.batches || []).filter(b => b.state === 'running' && b.process?.active === true);
              if (running.length === 0) {
                const queued = (proactiveStatus.batches || []).filter(b => b.state === 'reserved' || b.state === 'planned');
                if (queued.length === 0) {
                  return (
                    <div className="text-center text-gray-400 py-4">
                      <Upload className="w-8 h-8 mx-auto mb-2 opacity-40" />
                      <p className="text-sm">暂无活跃批次</p>
                      <p className="text-xs mt-1">启动任务后将按额度预留批次执行</p>
                    </div>
                  );
                }
                return (
                  <div className="space-y-3">
                    {queued.map(batch => (
                      <div key={batch.id} className="border border-amber-100 bg-amber-50 rounded-lg px-3 py-2.5">
                      <div className="flex items-center justify-between text-xs">
                        <span className="font-medium text-gray-700">批次 #{batch.id} 等待启动</span>
                        <span className="text-amber-700">{batch.remote || batch.account || '-'} · {formatBytes(batch.reserved_bytes || 0)}</span>
                      </div>
                      {(batch.file_paths || []).length > 0 && (
                        <div className="mt-1.5 space-y-0.5 text-[10px] text-gray-500">
                          {batch.file_paths.map(path => (
                            <div key={path} className="truncate" title={path}>{path.split('/').pop() || path}</div>
                          ))}
                        </div>
                      )}
                      </div>
                    ))}
                  </div>
                );
              }
              return (
                <div className="space-y-3">
                  {running.map(batch => (
                    <div key={batch.id} className="border border-blue-100 bg-blue-50 rounded-lg px-3 py-2.5">
                      <div className="flex items-center justify-between text-xs mb-1.5">
                        <span className="font-medium text-blue-700">批次 #{batch.id} 传输中</span>
                        <span className="text-blue-600">{batch.remote || batch.account || '-'} · {formatBytes(batch.reserved_bytes || 0)}</span>
                      </div>
                      {(batch.file_paths || []).length > 0 && (
                        <div className="text-[10px] text-gray-500 mb-1.5 space-y-0.5">
                          {(batch.file_paths || []).slice(0, 5).map((p, i) => (
                            <div key={i} className="truncate" title={p}>{p.split('/').pop() || p}</div>
                          ))}
                        </div>
                      )}
                      <div className="flex-1 h-2 bg-blue-100 rounded-full overflow-hidden">
                        <div className="h-full bg-blue-500 rounded-full animate-pulse" style={{ width: '60%' }} />
                      </div>
                      <div className="flex justify-between text-[10px] text-gray-500 mt-1">
                        <span>{batch.transfer_mode === 'move' ? '移动' : '复制'}{batch.started_at && ` · ${new Date(batch.started_at).toLocaleTimeString()}`}</span>
                      </div>
                    </div>
                  ))}
                </div>
              );
            })()
          ) : Object.keys(fileProgresses).length === 0 ? (
            <div className="text-center text-gray-400 py-4">
              <Upload className="w-8 h-8 mx-auto mb-2 opacity-40" />
              <p className="text-sm">暂无活跃传输</p>
              <p className="text-xs mt-1">启动任务后将显示实时进度</p>
            </div>
          ) : (
            Object.entries(fileProgresses).map(([fileName, data]) => (
              <div key={fileName} className="space-y-2">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2 min-w-0">
                    <File className="w-4 h-4 text-gray-400 flex-shrink-0" />
                    <span className="text-sm font-medium text-gray-700 truncate" title={fileName}>
                      {fileName}
                    </span>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="flex-1 h-2.5 bg-gray-100 rounded-full overflow-hidden">
                    <div
                      className="h-full bg-gradient-to-r from-blue-500 to-blue-400 rounded-full transition-all duration-500 ease-out"
                      style={{ width: `${Math.min(data.progress, 100)}%` }}
                    />
                  </div>
                  <span className="text-xs font-semibold text-blue-600 w-12 text-right flex-shrink-0">
                    {Math.min(data.progress, 100).toFixed(1)}%
                  </span>
                </div>
              </div>
            ))
          )}
        </div>
      </div>

    </div>
  );
};

const StatusBadge = ({ status }) => {
  const configs = {
    running: { text: '运行中', class: 'bg-green-100 text-green-700' },
    idle: { text: '当前空闲', class: 'bg-gray-100 text-gray-600' },
    paused: { text: '暂停', class: 'bg-amber-100 text-amber-700' },
    pausing: { text: '暂停中', class: 'bg-amber-100 text-amber-700' },
    canceled: { text: '已停止', class: 'bg-slate-100 text-slate-600' },
    error: { text: '异常', class: 'bg-red-100 text-red-700' },
  };

  const config = configs[status] || configs.idle;

  return (
    <span className={`px-2.5 py-1 rounded-full text-xs font-medium ${config.class}`}>
      {config.text}
    </span>
  );
};

const InfoCard = ({ icon: Icon, label, value, sub }) => (
  <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-4">
    <div className="flex items-center gap-2 mb-2">
      <Icon className="w-4 h-4 text-gray-400" />
      <span className="text-sm text-gray-500">{label}</span>
    </div>
    <div className="text-lg font-semibold text-gray-900">{value}</div>
    <div className="text-xs text-gray-500 mt-0.5">{sub}</div>
  </div>
);

const QBQueuePanel = ({ status }) => {
  const waiting = status?.waiting || [];
  const active = status?.active;
  const lastSync = status?.last_sync ? formatDateTime(status.last_sync) : '等待轮询';

  return (
    <div className="bg-white rounded-xl shadow-sm border border-emerald-100 overflow-hidden">
      <div className="px-6 py-4 border-b border-emerald-50 bg-gradient-to-r from-emerald-50 to-white flex flex-col md:flex-row md:items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <CheckCircle2 className="w-5 h-5 text-emerald-500" />
          <div>
            <h2 className="font-semibold text-gray-900">qBittorrent 传输队列</h2>
            <p className="text-xs text-gray-500 mt-0.5">只传输已完成种子的实际路径，队列按顺序逐个执行</p>
          </div>
        </div>
        <div className="flex flex-wrap gap-2 text-xs font-medium">
          <span className="px-2.5 py-1 rounded-full bg-white text-gray-700 border border-gray-200">qB 总数 {status?.total_torrents ?? 0}</span>
          <span className="px-2.5 py-1 rounded-full bg-emerald-100 text-emerald-700">完成 {status?.completed_count ?? 0}</span>
          <span className="px-2.5 py-1 rounded-full bg-blue-100 text-blue-700">匹配源目录 {status?.matched_completed ?? 0}</span>
        </div>
      </div>

      <div className="p-4 md:p-6 grid grid-cols-1 lg:grid-cols-2 gap-4">
        <div className="rounded-lg border border-gray-100 p-4 bg-gray-50/60">
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2 text-sm font-semibold text-gray-900">
              <Upload className="w-4 h-4 text-emerald-500" />
              正在传输
            </div>
            {active && <span className="px-2 py-0.5 rounded-full bg-emerald-100 text-emerald-700 text-xs animate-pulse">运行中</span>}
          </div>
          {active ? (
            <TorrentQueueItem item={active} />
          ) : (
            <div className="text-sm text-gray-400 py-6 text-center">当前没有正在传输的 qB 种子</div>
          )}
        </div>

        <div className="rounded-lg border border-gray-100 p-4 bg-gray-50/60">
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center gap-2 text-sm font-semibold text-gray-900">
              <Clock className="w-4 h-4 text-blue-500" />
              等待传输
            </div>
            <span className="px-2 py-0.5 rounded-full bg-blue-100 text-blue-700 text-xs">{status?.waiting_count ?? 0} 个</span>
          </div>
          {waiting.length > 0 ? (
            <div className="space-y-2 max-h-64 overflow-auto pr-1">
              {waiting.map((item, index) => (
                <TorrentQueueItem key={item.hash || `${item.name}-${index}`} item={item} index={index + 1} />
              ))}
            </div>
          ) : (
            <div className="text-sm text-gray-400 py-6 text-center">暂无等待中的完成种子</div>
          )}
        </div>
      </div>

      <div className="px-6 py-3 bg-gray-50 border-t border-gray-100 flex flex-col md:flex-row md:items-center justify-between gap-2 text-xs text-gray-500">
        <span>上次获取 qB：{lastSync}</span>
        {status?.last_error ? <span className="text-red-500">错误：{status.last_error}</span> : <span>轮询间隔：{status?.poll_interval || 60} 秒</span>}
      </div>
    </div>
  );
};

const PROACTIVE_STATUS_LABELS = {
  idle: '待机',
  running: '执行中',
  paused: '暂停',
  canceled: '已停止',
  error: '异常',
};

const BATCH_STATE_LABELS = {
  planned: '已计划',
  reserved: '已预留',
  running: '运行中',
  unknown: '待确认',
  reconciling: '对账中',
  succeeded: '已完成',
  failed: '失败',
  canceled: '已取消',
  expired: '已过期',
};

const TRANSFER_MODE_LABELS = {
  copy: '复制',
  move: '移动',
};

const COMPLETION_EVIDENCE_LABELS = {
  remote_verified: '远端已核验',
  local_move: '本地 / rclone 已完成',
};

const MERGE_BLOCKER_LABELS = {
  maintenance_epoch: '上一次合并仍在处理或等待确认。',
  scanner_active: '当前有任务正在处理，请稍后再试。',
  active_batch: '当前还有未完成的传输批次。',
  account_active_elsewhere: '相关账号正在其他任务中使用。',
  ledger_unavailable: '状态暂时无法确认，请稍后重试。',
};

const getManualMergeStatus = (maintenance = {}) => {
  if (maintenance.manual_merge_available) return { key: 'available', label: '可开始' };
  if (maintenance.dedupe_state === 'pending' || maintenance.dedupe_state === 'claimed' || maintenance.dedupe_state === 'running') return { key: 'running', label: '合并中' };
  if (maintenance.dedupe_state === 'unknown') return { key: 'unknown', label: '状态待确认' };
  return { key: 'unavailable', label: '暂不可用' };
};

const MANUAL_MERGE_RESULT_LABELS = {
  succeeded: '上次合并已完成',
  failed: '上次合并失败',
};

const getCompletionEvidenceLabel = (evidence, mode, state) => {
  if (COMPLETION_EVIDENCE_LABELS[evidence]) return COMPLETION_EVIDENCE_LABELS[evidence];
  if (state === 'succeeded' && mode === 'copy') return '远端已核验';
  if (state === 'succeeded' && mode === 'move') return '本地 / rclone 已完成';
  if (state === 'unknown' && mode === 'move') return '待人工处理';
  return '处理中';
};

const getResolutionItems = (batch, task) => {
  if (Array.isArray(batch.resolution_items)) {
    return batch.resolution_items.map(item => {
      const batchId = item.batch_id ?? batch.id;
      const sourceActions = item.actions;
      const actions = Array.isArray(sourceActions)
        ? sourceActions.filter(action => action === 'accept_moved' || action === 'restore_and_release')
        : [];
      if (String(batchId) !== String(batch.id) || item.eligible === false || item.available === false || !actions.length || item.file_id == null || !item.expected_state || !item.expected_updated_at) {
        return null;
      }
      return {
        batchId,
        actions,
        fileId: item.file_id,
        expectedState: item.expected_state,
        expectedUpdatedAt: item.expected_updated_at,
      };
    }).filter(Boolean);
  }

  const source = batch.resolution_availability || batch.resolution || {};
  const explicit = source.actions || batch.resolution_actions || batch.available_resolution_actions;
  const actions = Array.isArray(explicit)
    ? explicit.filter(action => action === 'accept_moved' || action === 'restore_and_release')
    : (source.available === true || batch.resolution_available === true || (task?.resolution_available === true && batch.state === 'unknown')
      ? ['accept_moved', 'restore_and_release']
      : []);
  const fileId = source.file_id ?? batch.resolution_file_id;
  const expectedState = source.expected_state ?? batch.resolution_expected_state;
  const expectedUpdatedAt = source.expected_updated_at ?? batch.resolution_expected_updated_at;
  if (!actions.length || fileId == null || !expectedState || !expectedUpdatedAt) return [];
  return [{ batchId: batch.id, actions, fileId, expectedState, expectedUpdatedAt }];
};

const ProactiveQuotaPanel = ({ status, loading, error, onRetry, taskId, onManualReset, resolutionState, onResolve, legacyRecoveryState, onRecover }) => {
  if (loading && !status) {
    return (
      <section className="bg-white rounded-xl shadow-sm border border-emerald-200 p-6" aria-live="polite" aria-busy="true">
        <div className="flex items-center gap-3 text-sm text-gray-600">
          <div className="h-4 w-4 rounded-full border-2 border-emerald-600 border-t-transparent animate-spin" />
          正在获取主动额度账号池状态...
        </div>
      </section>
    );
  }

  if (error && !status) {
    return (
      <section className="bg-white rounded-xl shadow-sm border border-red-200 p-6" role="alert">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <div>
            <h2 className="font-semibold text-gray-900">主动额度账号池状态不可用</h2>
            <p className="text-sm text-red-700 mt-1">{error}</p>
          </div>
          <button type="button" onClick={onRetry} className="inline-flex items-center justify-center px-3 py-2 min-h-10 bg-red-50 text-red-700 rounded-lg hover:bg-red-100 focus:outline-none focus:ring-2 focus:ring-red-500 font-medium text-sm">
            重试
          </button>
        </div>
      </section>
    );
  }

  if (!status) return null;

  const accounts = status.accounts || [];
  const batches = status.batches || [];
  const queue = status.queue || {};
  const maintenance = status.maintenance || {};
  const legacyRecovery = maintenance.legacy_recovery;
  const manualMergeStatus = getManualMergeStatus(maintenance);
  const manualMergeResult = MANUAL_MERGE_RESULT_LABELS[maintenance.result];
  const manualMergeError = maintenance.result === 'succeeded' ? '' : maintenance.error;
  const taskStatus = status.task?.status || 'idle';
  const queueCount = (queue.pending?.count || 0) + (queue.planned?.count || 0) + (queue.executing?.count || 0);
  const resolvedAccounts = accounts.filter(account => account.budget_bytes != null && account.remaining_bytes != null);
  const reservedBytes = resolvedAccounts.reduce((total, account) => total + (account.active_reserved_bytes || 0), 0);
  const activeBatch = batches.find(batch => batch.state === 'running' && batch.process?.active === true) || null;
  const transferMode = status.task?.transfer_mode || activeBatch?.transfer_mode || 'copy';
  const unknownMoveBatches = batches.filter(batch => batch.state === 'unknown' && batch.transfer_mode === 'move');
  const resolutionRows = unknownMoveBatches.flatMap(batch => {
    const items = getResolutionItems(batch, status.task);
    return (items.length ? items : [null]).map((availability, index) => ({ batch, availability, index }));
  });
  const batchSummary = Object.keys(BATCH_STATE_LABELS).map(state => ({
    state,
    count: batches.filter(batch => batch.state === state).length,
  })).filter(item => item.count > 0);

  return (
    <section className="bg-white rounded-xl shadow-sm border border-emerald-200 overflow-hidden" aria-labelledby="proactive-quota-heading">
      <div className="px-6 py-4 border-b border-emerald-100 bg-emerald-50/60 flex flex-col md:flex-row md:items-center justify-between gap-3">
        <div className="flex items-start gap-2">
          <ShieldCheck className="w-5 h-5 text-emerald-600 mt-0.5" />
          <div>
            <h2 id="proactive-quota-heading" className="font-semibold text-gray-900">主动额度账号池</h2>
            <p className="text-xs text-gray-600 mt-0.5">{TRANSFER_MODE_LABELS[transferMode] || transferMode}模式 · {transferMode === 'move' ? '完成以本地 / rclone 证据为准' : '完成后远端已核验'}</p>
          </div>
        </div>
        <span className="text-xs font-medium text-emerald-800 bg-white border border-emerald-200 rounded-full px-2.5 py-1">
          {PROACTIVE_STATUS_LABELS[taskStatus] || taskStatus}
        </span>
      </div>

      {(error || loading) && (
        <div className={`px-6 py-2 text-xs flex flex-wrap items-center justify-between gap-2 ${error ? 'bg-red-50 text-red-700' : 'bg-gray-50 text-gray-600'}`} aria-live="polite">
          <span>{error ? `状态刷新失败：${error}，当前显示最近一次成功数据。` : '正在刷新状态...'}</span>
          {error && <button type="button" onClick={onRetry} className="font-medium underline focus:outline-none focus:ring-2 focus:ring-red-500 rounded">重试</button>}
        </div>
      )}

      <div className="px-6 py-3 border-b border-gray-100 flex flex-col sm:flex-row sm:items-center justify-between gap-2" aria-live="polite">
        <div className="min-w-0">
          <div className="text-sm font-medium text-gray-900">手动合并</div>
          <div className="text-xs text-gray-500 mt-0.5">
            {manualMergeStatus.key === 'unavailable'
              ? (MERGE_BLOCKER_LABELS[maintenance.blocker] || '当前暂不可开始合并。')
              : manualMergeStatus.key === 'unknown'
                ? '上次合并结果暂时无法确认，请先刷新状态。'
                : manualMergeStatus.key === 'running'
                  ? '合并正在处理，请等待状态更新。'
                  : '仅在你明确操作时执行合并。'}
          </div>
          {(manualMergeResult || manualMergeError) && (
            <div className={`text-xs mt-1 ${maintenance.result === 'failed' || manualMergeError ? 'text-red-700' : 'text-emerald-700'}`}>
              {manualMergeResult || '最近一次合并状态'}
              {manualMergeError ? `：${manualMergeError.slice(0, 240)}` : ''}
            </div>
          )}
        </div>
        <span className={`self-start sm:self-auto shrink-0 text-xs font-medium rounded-full px-2.5 py-1 ${
          manualMergeStatus.key === 'available' ? 'bg-emerald-100 text-emerald-800' :
            manualMergeStatus.key === 'running' ? 'bg-blue-100 text-blue-800' :
              manualMergeStatus.key === 'unknown' ? 'bg-amber-100 text-amber-800' : 'bg-gray-100 text-gray-700'
        }`}>
          {manualMergeStatus.label}
        </span>
      </div>

      {maintenance.blocker === 'legacy_maintenance_recovery' && legacyRecovery && (
        <div className="mx-4 mt-4 md:mx-6 border border-amber-200 bg-amber-50 rounded-lg p-4" role="region" aria-labelledby="legacy-recovery-heading">
          <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
            <div className="min-w-0">
              <h3 id="legacy-recovery-heading" className="text-sm font-semibold text-amber-950">需要恢复任务状态</h3>
              <p className="text-xs text-amber-900 mt-1">
                {legacyRecovery.dedupe_state === 'unknown'
                  ? legacyRecovery.process_identity_available
                    ? '相关处理已无法自动确认。确认它已经停止后，可以释放当前阻塞。'
                    : '当前无法安全确认相关处理是否已停止，请稍后重试。'
                  : '当前状态已变化，请刷新后再查看。'}
              </p>
              {legacyRecoveryState.error && <p className="text-xs text-red-700 mt-2" role="alert">{legacyRecoveryState.error}</p>}
            </div>
            {legacyRecovery.dedupe_state === 'unknown' && legacyRecovery.process_identity_available && (
              <button
                type="button"
                onClick={() => onRecover(legacyRecovery)}
                disabled={legacyRecoveryState.loading}
                aria-label="确认已停止并恢复任务状态"
                title="确认已停止并恢复任务状态"
                className="inline-flex items-center justify-center min-h-11 sm:min-h-10 px-3 py-2 text-xs font-medium rounded-lg bg-amber-700 text-white hover:bg-amber-800 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus:ring-2 focus:ring-amber-600"
              >
                {legacyRecoveryState.loading ? '处理中...' : '确认已停止并恢复'}
              </button>
            )}
          </div>
        </div>
      )}

      <div className="p-4 md:p-6 space-y-4">
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
          <RuntimeMetric label="队列" value={queueCount} sub="待处理文件" icon={ListChecks} />
          <RuntimeMetric label="当前运行" value={activeBatch ? 1 : 0} sub="执行中的批次" icon={Upload} />
          <RuntimeMetric label="账号" value={`${resolvedAccounts.length}/${accounts.length}`} sub="已初始化 / 绑定" icon={Database} />
          <RuntimeMetric label="今日预留" value={formatBytes(reservedBytes)} sub="当前活跃预留" icon={ShieldCheck} />
        </div>

        {batchSummary.length > 0 && (
          <div className="flex flex-wrap gap-2" aria-label="批次状态汇总">
            {batchSummary.map(item => (
              <span key={item.state} className="text-xs font-medium text-gray-700 bg-gray-100 rounded-full px-2.5 py-1">
                {BATCH_STATE_LABELS[item.state]} {item.count}
              </span>
            ))}
          </div>
        )}

        {(activeBatch || accounts.length > 0 || batches.length > 0) && (
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            {activeBatch && (
              <div className="border border-gray-200 rounded-lg p-4">
                <div className="flex items-center justify-between mb-3">
                  <h3 className="text-sm font-semibold text-gray-900">当前批次</h3>
                  <span className="text-xs text-blue-700 bg-blue-50 rounded-full px-2 py-0.5">{BATCH_STATE_LABELS[activeBatch.state] || activeBatch.state}</span>
                </div>
                <dl className="grid grid-cols-2 gap-3 text-xs">
                  <div><dt className="text-gray-500">账号</dt><dd className="font-medium text-gray-800 mt-0.5">{activeBatch.remote || activeBatch.account || activeBatch.destination_remote || '-'}</dd></div>
                  <div><dt className="text-gray-500">批次 ID</dt><dd className="font-mono text-gray-800 mt-0.5">{activeBatch.id || '-'}</dd></div>
                  <div><dt className="text-gray-500">文件数</dt><dd className="font-medium text-gray-800 mt-0.5">{Object.values(activeBatch.file_counts || {}).reduce((total, item) => total + (item.count || 0), 0) || '-'}</dd></div>
                  <div><dt className="text-gray-500">预留额度</dt><dd className="font-medium text-gray-800 mt-0.5">{formatBytes(activeBatch.reserved_bytes || 0)}</dd></div>
                  <div><dt className="text-gray-500">模式</dt><dd className="font-medium text-gray-800 mt-0.5">{TRANSFER_MODE_LABELS[activeBatch.transfer_mode || transferMode] || activeBatch.transfer_mode || transferMode}</dd></div>
                  <div><dt className="text-gray-500">完成证据</dt><dd className="font-medium text-gray-800 mt-0.5">{getCompletionEvidenceLabel(activeBatch.completion_evidence, activeBatch.transfer_mode || transferMode, activeBatch.state)}</dd></div>
                </dl>
              </div>
            )}

            {batches.length > 0 && (
              <div className="border border-gray-200 rounded-lg p-4">
                <h3 className="text-sm font-semibold text-gray-900 mb-3">最近批次</h3>
                <div className="space-y-2 max-h-48 overflow-auto">
                  {batches.slice(0, 6).map(batch => (
                    <div key={batch.id} className="flex items-start justify-between gap-3 text-xs">
                      <div className="min-w-0">
                        <div className="font-medium text-gray-800">#{batch.id} · {BATCH_STATE_LABELS[batch.state] || batch.state}</div>
                        <div className="text-gray-500 truncate">{batch.remote || batch.account || '未绑定账号'}</div>
                        <div className="text-gray-500">{TRANSFER_MODE_LABELS[batch.transfer_mode] || batch.transfer_mode || TRANSFER_MODE_LABELS[transferMode]} · {getCompletionEvidenceLabel(batch.completion_evidence, batch.transfer_mode || transferMode, batch.state)}</div>
                        {batch.error && <div className="text-red-700 break-words mt-0.5">{batch.error.slice(0, 200)}</div>}
                      </div>
                      <span className="text-gray-500 whitespace-nowrap">{formatBytes(batch.reserved_bytes || 0)}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}

            {resolutionRows.length > 0 && (
              <div className="lg:col-span-2 border border-amber-300 bg-amber-50 rounded-lg p-4" role="region" aria-labelledby="move-resolution-heading">
                <div className="flex items-start gap-3">
                  <div className="min-w-0 flex-1">
                    <h3 id="move-resolution-heading" className="text-sm font-semibold text-amber-950">移动批次需要处理</h3>
                    <p className="text-xs text-amber-900 mt-1">这些批次的本地结果无法自动确认。仅使用后端提供的安全处理动作，不显示文件路径或令牌。</p>
                  </div>
                </div>
                <div className="mt-3 space-y-3">
                  {resolutionRows.map(({ batch, availability, index }) => (
                    <MoveResolutionRow
                      key={`${batch.id}-${availability?.fileId ?? `pending-${index}`}`}
                      batch={batch}
                      availability={availability}
                      itemIndex={index}
                      resolutionState={resolutionState}
                      onResolve={onResolve}
                    />
                  ))}
                </div>
              </div>
            )}

            {accounts.length > 0 && (
              <div className="border border-gray-200 rounded-lg p-4">
                <h3 className="text-sm font-semibold text-gray-900 mb-3">账号额度</h3>
                <div className="space-y-3 max-h-72 overflow-auto">
                  {accounts.map((account, index) => (
                    <div key={account.account_id ?? index} className="space-y-1.5">
                      <div className="flex flex-col sm:flex-row sm:items-baseline justify-between gap-2 text-xs min-w-0">
                        <div className="min-w-0">
                          <span className="font-medium text-gray-800 break-words">{account.remote_name || `账号 ${index + 1}`}</span>
                          {account.budget_bytes == null || account.remaining_bytes == null ? (
                            <span className="block text-amber-700 mt-0.5">未初始化 quota account</span>
                          ) : account.enabled === false ? (
                            <span className="block text-gray-500 mt-0.5">已禁用</span>
                          ) : account.availability_state === 'campaign_cooldown' ? (
                            <span className="block text-amber-700 mt-0.5">本地 24 小时活动冷却中 · 最早可用 {account.next_recovery_at ? formatDateTime(account.next_recovery_at) : account.cooldown_until ? formatDateTime(account.cooldown_until) : '等待状态更新'}</span>
                          ) : account.availability_state === 'provider_blocked' || isFutureTimestamp(account.provider_blocked_until) ? (
                            <span className="block text-red-700 mt-0.5">Provider 暂时阻断</span>
                          ) : null}
                        </div>
                      </div>
                      <QuotaAccountBar account={account} taskId={taskId} onManualReset={onManualReset} />
                    </div>
                  ))}
                </div>
                <div className="mt-3">
                  <QuotaExhaustedNotice status={status} />
                </div>
              </div>
            )}
          </div>
        )}
        <NetworkTelemetry telemetry={status.network_telemetry} />
      </div>
    </section>
  );
};

const NetworkTelemetry = ({ telemetry }) => {
  if (!telemetry || typeof telemetry !== 'object') return null;
  const available = telemetry.available === true;
  const baselineAvailable = telemetry.baseline_available === true;
  const formatTelemetryBytes = (value) => {
    if (value == null || !Number.isFinite(Number(value))) return '不可用';
    const bytes = Math.max(0, Number(value));
    if (bytes === 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1000)), units.length - 1);
    return `${parseFloat((bytes / (1000 ** exponent)).toFixed(2))} ${units[exponent]}`;
  };
  const formatDifference = (value) => {
    if (value == null || !Number.isFinite(Number(value))) return '不可用';
    const numericValue = Number(value);
    return `${numericValue >= 0 ? '+' : '-'}${formatTelemetryBytes(Math.abs(numericValue))}`;
  };
  const fields = available ? [
    ['tx_bytes', '本次发送', formatTelemetryBytes(telemetry.tx_bytes)],
    ['rolling_24h_tx_bytes', '发送（滚动24小时）', formatTelemetryBytes(telemetry.rolling_24h_tx_bytes)],
    ['ledger_committed_bytes', '账本已提交', formatTelemetryBytes(telemetry.ledger_committed_bytes)],
    ['difference_bytes', '账本差值', formatDifference(telemetry.difference_bytes)],
    ['baseline_available', '基线', baselineAvailable ? '可用' : '不可用'],
    ['baseline_at', '基线时间', baselineAvailable && telemetry.baseline_at ? formatDateTime(telemetry.baseline_at) : '不可用'],
    ['sampled_at', '采样时间', telemetry.sampled_at ? formatDateTime(telemetry.sampled_at) : '不可用'],
  ] : [];
  return (
    <section className="mt-4 border-t border-gray-200 pt-4" aria-labelledby="network-telemetry-heading">
      <div className="flex items-start gap-2">
        <Network className="mt-0.5 h-4 w-4 shrink-0 text-gray-500" />
        <div className="min-w-0">
          <h3 id="network-telemetry-heading" className="text-sm font-semibold text-gray-800">网络观测</h3>
          <p className="mt-0.5 text-xs text-gray-500">只读遥测对比，不参与额度判断或执行。</p>
          {!available ? (
            <p className="mt-2 text-xs text-gray-500">网络遥测当前不可用。</p>
          ) : (
            <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-2 text-xs sm:grid-cols-3 lg:grid-cols-4">
              {fields.map(([key, label, value]) => <div key={key} className="min-w-0"><dt className="text-gray-500">{label}</dt><dd className="mt-0.5 break-words font-medium text-gray-800">{value}</dd></div>)}
            </dl>
          )}
        </div>
      </div>
    </section>
  );
};

const RuntimeMetric = ({ label, value, sub, icon: Icon }) => (
  <div className="border border-gray-200 rounded-lg p-3">
    <div className="flex items-center gap-2 text-xs text-gray-500"><Icon className="w-4 h-4 text-emerald-600" />{label}</div>
    <div className="text-lg font-semibold text-gray-900 mt-1">{value}</div>
    <div className="text-xs text-gray-500 mt-0.5">{sub}</div>
  </div>
);

const MoveResolutionRow = ({ batch, availability, itemIndex, resolutionState, onResolve }) => {
  const actions = availability?.actions || [];
  const resolutionKey = availability ? `${batch.id}:${availability.fileId}` : '';
  const isCurrent = resolutionState.batchId != null && `${resolutionState.batchId}:${resolutionState.fileId}` === resolutionKey;
  const isLoading = isCurrent && resolutionState.loading;
  const error = isCurrent ? resolutionState.error : '';
  const success = isCurrent ? resolutionState.success : '';
  return (
    <div className="bg-white border border-amber-200 rounded-md p-3">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-sm font-medium text-gray-900">批次 #{batch.id} · 项目 {itemIndex + 1}</div>
          <div className="text-xs text-gray-600 mt-0.5">移动结果未知 · 需要明确处理</div>
        </div>
        <div className="flex flex-wrap gap-2">
          {actions.includes('accept_moved') && (
            <button
              type="button"
              disabled={isLoading}
              onClick={() => onResolve(batch.id, 'accept_moved', availability)}
              className="min-h-10 px-3 py-2 text-xs font-medium rounded-lg bg-amber-700 text-white hover:bg-amber-800 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus:ring-2 focus:ring-amber-600"
            >
              {isLoading && resolutionState.action === 'accept_moved' ? '处理中...' : '确认已移动'}
            </button>
          )}
          {actions.includes('restore_and_release') && (
            <button
              type="button"
              disabled={isLoading}
              onClick={() => onResolve(batch.id, 'restore_and_release', availability)}
              className="min-h-10 px-3 py-2 text-xs font-medium rounded-lg border border-gray-300 text-gray-700 hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus:ring-2 focus:ring-blue-500"
            >
              {isLoading && resolutionState.action === 'restore_and_release' ? '处理中...' : '恢复并释放额度'}
            </button>
          )}
        </div>
      </div>
      {actions.length === 0 && <p className="text-xs text-gray-600 mt-2">等待后端提供可用的安全处理动作。</p>}
      {error && <p className="text-xs text-red-700 mt-2" role="alert">{error}</p>}
      {success && <p className="text-xs text-emerald-700 mt-2" role="status">{success}</p>}
    </div>
  );
};

const isFutureTimestamp = (value) => {
  if (!value) return false;
  const timestamp = new Date(value).getTime();
  return Number.isFinite(timestamp) && timestamp > Date.now();
};

const TorrentQueueItem = ({ item, index }) => (
  <div className="rounded-lg bg-white border border-gray-100 p-3">
    <div className="flex items-center gap-2 min-w-0">
      {index && <span className="text-xs font-semibold text-blue-600 bg-blue-50 rounded-full px-2 py-0.5">#{index}</span>}
      <File className="w-4 h-4 text-gray-400 flex-shrink-0" />
      <span className="text-sm font-medium text-gray-800 truncate" title={item.name}>{item.name || '未命名种子'}</span>
    </div>
    <div className="mt-2 text-xs text-gray-500 font-mono break-all">{item.source_path || '-'}</div>
  </div>
);

const formatBytes = (bytes) => {
  if (bytes === 0 || !bytes) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
};

const formatSpeed = (bytesPerSec) => {
  if (bytesPerSec === 0 || !bytesPerSec) return '0 B/s';
  return formatBytes(bytesPerSec) + '/s';
};

const formatDateTime = (value) => {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
};

export default TaskDetail;

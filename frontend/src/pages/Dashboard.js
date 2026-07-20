import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { 
  Play, 
  Square, 
  HardDrive, 
  Clock, 
  Activity,
  ArrowRight,
  AlertCircle,
  X,
  Loader2,
} from 'lucide-react';
import { getTasks, getSystemStats, startTask, stopTask, getQuickTasks, deleteTask } from '../services/api';
import { createWebSocket } from '../services/api';
import toast from 'react-hot-toast';

const Dashboard = () => {
  const [tasks, setTasks] = useState([]);
  const [stats, setStats] = useState({ total_tasks: 0, running_tasks: 0 });
  const [loading, setLoading] = useState(true);
  const [wsConnected, setWsConnected] = useState(false);
  const [quickTasks, setQuickTasks] = useState([]);
  const [quickTaskProgress, setQuickTaskProgress] = useState({});
  const [deletingQuickTaskId, setDeletingQuickTaskId] = useState(null);

  useEffect(() => {
    loadData();

    // WebSocket connection
    const ws = createWebSocket();

    ws.onopen = () => {
      setWsConnected(true);
    };

    ws.onclose = () => {
      setWsConnected(false);
    };

    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        if (data.type === 'file_progress' && data.task_id) {
          setQuickTaskProgress((prev) => ({
            ...prev,
            [data.task_id]: {
              progress: data.progress || 0,
              fileName: data.file_name || '',
              bytes: data.bytes || 0,
              size: data.size || 0,
              speed: data.speed || 0,
            },
          }));
          setQuickTasks((prev) => prev.map((task) => (
            task.id === data.task_id ? { ...task, status: 'running' } : task
          )));
          return;
        }
        if (!data.task_id) return;
        if (data.type === 'task_started') {
          setQuickTasks((prev) => prev.map((task) => (
            task.id === data.task_id ? { ...task, status: 'running', last_error: '' } : task
          )));
        } else if (data.type === 'task_complete') {
          setQuickTasks((prev) => prev.map((task) => (
            task.id === data.task_id ? { ...task, status: 'idle', last_error: '' } : task
          )));
          setQuickTaskProgress((prev) => ({
            ...prev,
            [data.task_id]: { ...(prev[data.task_id] || {}), progress: 100, speed: 0 },
          }));
          loadQuickTasks();
        } else if (data.type === 'task_error') {
          setQuickTasks((prev) => prev.map((task) => (
            task.id === data.task_id ? { ...task, status: 'error', last_error: data.error || '任务失败' } : task
          )));
          loadQuickTasks();
        } else if (data.type === 'task_stopped') {
          setQuickTasks((prev) => prev.map((task) => (
            task.id === data.task_id ? { ...task, status: 'idle' } : task
          )));
          loadQuickTasks();
        }
        if (data.type === 'task_complete' || data.type === 'task_error' ||
            data.type === 'task_started' || data.type === 'task_stopped') {
          loadData();
        }
      } catch {
        // ignore malformed messages
      }
    };

    // Refresh every 2 seconds (was 5s — too slow to feel real-time)
    const interval = setInterval(loadData, 2000);
    loadQuickTasks();
    const quickInterval = setInterval(loadQuickTasks, 3000);

    return () => {
      ws.close();
      clearInterval(interval);
      clearInterval(quickInterval);
    };
  }, []);

  const loadData = async () => {
    try {
      const [tasksRes, statsRes] = await Promise.all([
        getTasks(),
        getSystemStats()
      ]);
      setTasks(tasksRes.data);
      setStats(statsRes.data);
    } catch (err) {
      console.error('Failed to load data:', err);
    } finally {
      setLoading(false);
    }
  };

  const loadQuickTasks = async () => {
    try {
      const res = await getQuickTasks();
      setQuickTasks(res.data || []);
    } catch {
      setQuickTasks([]);
    }
  };

  const handleStart = async (id) => {
    // Optimistically update local state
    setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'running' } : t));
    try {
      await startTask(id);
      toast.success('任务已启动');
      setTimeout(loadData, 300);
    } catch (err) {
      // Revert on failure
      setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'idle' } : t));
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async (id) => {
    setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'idle' } : t));
    try {
      await stopTask(id);
      toast.success('任务已停止');
      setTimeout(loadData, 300);
    } catch (err) {
      setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'running' } : t));
      toast.error('停止失败');
    }
  };

  const handleDeleteQuickTask = async (taskId) => {
    setDeletingQuickTaskId(taskId);
    try {
      await deleteTask(taskId);
      setQuickTasks((prev) => prev.filter((task) => task.id !== taskId));
      setQuickTaskProgress((prev) => {
        const next = { ...prev };
        delete next[taskId];
        return next;
      });
      toast.success('已删除');
    } catch (err) {
      toast.error(err.response?.data?.error || '删除失败');
    } finally {
      setDeletingQuickTaskId(null);
    }
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  const runningTasks = tasks.filter(t => t.status === 'running');
  const idleTasks = tasks.filter(t => t.status === 'idle');
  const errorTasks = tasks.filter(t => t.status === 'error');

  const runningQuickTasks = quickTasks.filter((task) => task.status === 'running');
  const finishedQuickTasks = quickTasks.filter((task) => task.status !== 'running' && (task.last_run || task.status === 'error'));

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl md:text-2xl font-bold text-gray-900">总览</h1>
          <p className="text-xs md:text-sm text-gray-500 mt-0.5 md:mt-1">Rclone 自动化任务管理面板</p>
        </div>
        <div className="flex items-center gap-1.5">
          <span className={`w-2 h-2 md:w-2.5 md:h-2.5 rounded-full ${wsConnected ? 'bg-green-500' : 'bg-red-500'}`}></span>
          <span className="text-xs md:text-sm text-gray-500">{wsConnected ? '实时连接' : '离线'}</span>
        </div>
      </div>

      {/* Stats Cards */}
      <div className="grid grid-cols-2 md:grid-cols-2 lg:grid-cols-4 gap-3 md:gap-4">
        <StatCard
          icon={HardDrive}
          label="总任务数"
          value={stats.total_tasks}
          color="blue"
        />
        <StatCard
          icon={Activity}
          label="运行中"
          value={stats.running_tasks}
          color="green"
        />
        <StatCard
          icon={Clock}
          label="待机中"
          value={idleTasks.length}
          color="yellow"
        />
        <StatCard
          icon={AlertCircle}
          label="异常"
          value={errorTasks.length}
          color="red"
        />
      </div>

      {/* Running Tasks */}
      {runningTasks.length > 0 && (
        <div className="bg-white rounded-xl shadow-sm border border-gray-200">
          <div className="px-4 md:px-6 py-3 md:py-4 border-b border-gray-100">
            <h2 className="font-semibold text-gray-900 flex items-center gap-2 text-sm md:text-base">
              <Play className="w-4 h-4 md:w-5 md:h-5 text-green-500" />
              正在运行 ({runningTasks.length})
            </h2>
          </div>
          <div className="divide-y divide-gray-100">
            {runningTasks.map(task => (
              <div key={task.id} className="px-4 md:px-6 py-3 md:py-4 flex items-center justify-between hover:bg-gray-50">
                <div className="min-w-0 flex-1 mr-2">
                  <div className="font-medium text-gray-900 text-sm md:text-base truncate">{task.name}</div>
                  <div className="text-xs md:text-sm text-gray-500 mt-0.5 truncate">
                    {task.source_dir} → {task.remote_name}:{task.remote_dir}
                  </div>
                </div>
                <div className="flex items-center gap-2 md:gap-3 flex-shrink-0">
                  <span className="px-2 md:px-2.5 py-0.5 md:py-1 bg-green-100 text-green-700 text-xs font-medium rounded-full">
                    运行中
                  </span>
                  <Link
                    to={`/tasks/${task.id}`}
                    className="text-blue-600 hover:text-blue-700 text-xs md:text-sm font-medium"
                  >
                    查看
                  </Link>
                  <button
                    onClick={() => handleStop(task.id)}
                    className="p-1 md:p-1.5 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                  >
                    <Square className="w-3.5 h-3.5 md:w-4 md:h-4" />
                  </button>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Task List */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200">
        <div className="px-4 md:px-6 py-3 md:py-4 border-b border-gray-100 flex items-center justify-between">
          <h2 className="font-semibold text-gray-900 text-sm md:text-base">任务列表</h2>
          <Link
            to="/tasks/new"
            className="text-xs md:text-sm text-blue-600 hover:text-blue-700 font-medium"
          >
            + 新建任务
          </Link>
        </div>

        {tasks.length === 0 ? (
          <div className="px-4 md:px-6 py-8 md:py-12 text-center">
            <HardDrive className="w-8 h-8 md:w-12 md:h-12 text-gray-300 mx-auto mb-2 md:mb-3" />
            <p className="text-xs md:text-sm text-gray-500">暂无任务，点击上方按钮创建</p>
          </div>
        ) : (
          <div className="divide-y divide-gray-100">
            {tasks.map(task => (
              <div key={task.id} className="px-4 md:px-6 py-3 md:py-4 flex items-center justify-between hover:bg-gray-50">
                <div className="flex-1 min-w-0 mr-2">
                  <div className="flex items-center gap-1.5">
                    <span className={`w-1.5 h-1.5 md:w-2 md:h-2 rounded-full flex-shrink-0 ${
                      task.status === 'running' ? 'bg-green-500' :
                      task.status === 'error' ? 'bg-red-500' : 'bg-gray-400'
                    }`}></span>
                    <span className="font-medium text-gray-900 text-sm md:text-base truncate">{task.name}</span>
                  </div>
                  <div className="text-xs md:text-sm text-gray-500 mt-0.5 truncate">
                    {task.source_dir} → {task.remote_name}:{task.remote_dir}
                  </div>
                  <div className="flex items-center gap-2 md:gap-3 mt-1 text-xs text-gray-400">
                    <span>并发: {task.transfers}</span>
                    <span>检查: {task.checkers}</span>
                    {task.watch_enabled && <span className="text-blue-500">监控</span>}
                    {task.schedule_enabled && <span className="text-purple-500">定时</span>}
                    {task.qb_enabled && !(task.task_type === 'rotation' && task.rotation_strategy === 'proactive_quota') && <span className="text-amber-500">qB完成</span>}
                  </div>
                </div>

                <div className="flex items-center gap-1 md:gap-2 ml-2 flex-shrink-0">
                  {task.status !== 'running' ? (
                    <button
                      onClick={() => handleStart(task.id)}
                      className="p-1.5 md:p-2 text-green-600 hover:bg-green-50 rounded-lg transition-colors"
                      title="启动"
                    >
                      <Play className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    </button>
                  ) : (
                    <button
                      onClick={() => handleStop(task.id)}
                      className="p-1.5 md:p-2 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                      title="停止"
                    >
                      <Square className="w-3.5 h-3.5 md:w-4 md:h-4" />
                    </button>
                  )}
                  <Link
                    to={`/tasks/${task.id}`}
                    className="p-1.5 md:p-2 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors"
                    title="详情"
                  >
                    <ArrowRight className="w-3.5 h-3.5 md:w-4 md:h-4" />
                  </Link>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Quick Tasks */}
      {(runningQuickTasks.length > 0 || finishedQuickTasks.length > 0) && (
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
          <div className="px-4 md:px-5 py-2.5 md:py-3 border-b border-gray-100 flex items-center justify-between">
            <h2 className="font-semibold text-gray-900 text-sm md:text-base">文件浏览器任务</h2>
            <Link to="/files" className="text-xs md:text-sm text-blue-600 hover:text-blue-700 font-medium">前往文件浏览器</Link>
          </div>

          {runningQuickTasks.length > 0 && (
            <div className="p-3 space-y-2">
              {runningQuickTasks.map((task) => {
                const progressInfo = quickTaskProgress[task.id] || {};
                const progress = Math.max(0, Math.min(100, Number(progressInfo.progress || 0)));
                return (
                  <Link
                    key={task.id}
                    to={`/tasks/${task.id}`}
                    className="block rounded-lg border border-blue-100 bg-blue-50/50 px-3 py-2.5 hover:border-blue-200 hover:bg-blue-50 transition-colors"
                  >
                    <div className="flex items-start justify-between gap-3 mb-1.5">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-gray-900 truncate leading-5">{task.name}</div>
                        <div className="text-[11px] text-gray-500 mt-0.5 break-all leading-4">{task.source_dir} → {task.dest_type === 'local' ? task.remote_dir : `${task.remote_name}:${task.remote_dir}`}</div>
                      </div>
                      <div className="text-xs font-semibold text-blue-700 shrink-0">{progress.toFixed(0)}%</div>
                    </div>
                    <div className="h-1.5 rounded-full bg-blue-100 overflow-hidden">
                      <div className="h-full bg-blue-600 rounded-full transition-all duration-300" style={{ width: `${progress}%` }} />
                    </div>
                    <div className="mt-1.5 text-[11px] text-gray-500 truncate leading-4">{progressInfo.fileName || '传输中'}</div>
                  </Link>
                );
              })}
            </div>
          )}

          {finishedQuickTasks.length > 0 && (
            <div className={`${runningQuickTasks.length > 0 ? 'border-t' : ''} p-3 space-y-1.5`}>
              {finishedQuickTasks.map((task) => {
                const isError = task.status === 'error';
                const isPaused = task.status === 'paused';
                const isCanceled = task.status === 'canceled';
                const cardClass = isError
                  ? 'border-red-200 bg-red-50'
                  : isPaused
                    ? 'border-amber-200 bg-amber-50'
                    : isCanceled
                      ? 'border-slate-200 bg-slate-50'
                      : 'border-green-200 bg-green-50';
                const textClass = isError
                  ? 'text-red-700'
                  : isPaused
                    ? 'text-amber-700'
                    : isCanceled
                      ? 'text-slate-700'
                      : 'text-green-700';
                const subClass = isError
                  ? 'text-red-500'
                  : isPaused
                    ? 'text-amber-600'
                    : isCanceled
                      ? 'text-slate-500'
                      : 'text-green-600';
                const statusText = isError
                  ? (task.last_error || '任务失败')
                  : isPaused
                    ? '已暂停'
                    : isCanceled
                      ? '已停止'
                      : '100%';
                return (
                  <div
                    key={task.id}
                    className={`flex items-center gap-2.5 rounded-lg border px-3 py-2 ${cardClass}`}
                  >
                    <Link to={`/tasks/${task.id}`} className="flex-1 min-w-0">
                      <div className={`text-sm font-medium truncate leading-5 ${textClass}`}>{task.name}</div>
                      <div className={`text-[11px] mt-0.5 truncate leading-4 ${subClass}`}>
                        {statusText}
                      </div>
                    </Link>
                    <button
                      type="button"
                      onClick={() => handleDeleteQuickTask(task.id)}
                      disabled={deletingQuickTaskId === task.id}
                      className={`p-1.5 rounded-lg transition-colors ${isError ? 'text-red-500 hover:bg-red-100' : 'text-green-600 hover:bg-green-100'} disabled:opacity-50`}
                    >
                      {deletingQuickTaskId === task.id ? <Loader2 className="w-4 h-4 animate-spin" /> : <X className="w-4 h-4" />}
                    </button>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}
    </div>
  );
};

const StatCard = ({ icon: Icon, label, value, color }) => {
  const colors = {
    blue: 'bg-blue-50 text-blue-600',
    green: 'bg-green-50 text-green-600',
    yellow: 'bg-yellow-50 text-yellow-600',
    red: 'bg-red-50 text-red-600',
  };

  return (
    <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-3 md:p-5">
      <div className="flex items-center justify-between">
        <div>
          <p className="text-xs md:text-sm font-medium text-gray-500">{label}</p>
          <p className="text-xl md:text-2xl font-bold text-gray-900 mt-0.5 md:mt-1">{value}</p>
        </div>
        <div className={`p-2 md:p-3 rounded-lg ${colors[color]}`}>
          <Icon className="w-4 h-4 md:w-6 md:h-6" />
        </div>
      </div>
    </div>
  );
};

export default Dashboard;

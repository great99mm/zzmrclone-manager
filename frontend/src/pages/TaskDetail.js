import React, { useState, useEffect, useRef } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { 
  ArrowLeft, 
  Play, 
  Square, 
  RotateCcw, 
  Pencil, 
  Trash2,
  Terminal,
  Activity,
  Clock,
  CheckCircle2,
  AlertCircle
} from 'lucide-react';
import { getTask, getTaskStatus, getTaskLogs, startTask, stopTask, dedupeTask, deleteTask } from '../services/api';
import { createWebSocket } from '../services/api';
import toast from 'react-hot-toast';

const TaskDetail = () => {
  const { id } = useParams();
  const navigate = useNavigate();
  const [task, setTask] = useState(null);
  const [logs, setLogs] = useState([]);
  const [status, setStatus] = useState({ status: 'idle', running: false });
  const [loading, setLoading] = useState(true);
  const [autoScroll, setAutoScroll] = useState(true);
  const logsEndRef = useRef(null);
  const wsRef = useRef(null);

  useEffect(() => {
    loadTask();
    loadLogs();
    loadStatus();

    // Setup WebSocket for real-time logs
    const ws = createWebSocket();
    wsRef.current = ws;

    ws.onmessage = (event) => {
      const data = JSON.parse(event.data);
      if (data.task_id === parseInt(id)) {
        if (data.type === 'log') {
          setLogs(prev => [...prev.slice(-500), {
            time: data.time,
            content: data.content,
            stream: data.stream,
          }]);
        } else if (data.type === 'task_complete') {
          toast.success('任务执行完成');
          loadStatus();
        } else if (data.type === 'task_error') {
          toast.error(`任务异常: ${data.error}`);
          loadStatus();
        }
      }
    };

    // Poll status every 3 seconds
    const interval = setInterval(() => {
      loadStatus();
    }, 3000);

    return () => {
      ws.close();
      clearInterval(interval);
    };
  }, [id]);

  useEffect(() => {
    if (autoScroll && logsEndRef.current) {
      logsEndRef.current.scrollIntoView({ behavior: 'smooth' });
    }
  }, [logs, autoScroll]);

  const loadTask = async () => {
    try {
      const res = await getTask(id);
      setTask(res.data);
    } catch (err) {
      toast.error('加载任务失败');
      navigate('/tasks');
    } finally {
      setLoading(false);
    }
  };

  const loadLogs = async () => {
    try {
      const res = await getTaskLogs(id, 200);
      const logContent = res.data.logs[0] || '';
      const lines = logContent.split('\n').filter(l => l.trim()).map(line => ({
        time: line.match(/\[(.*?)\]/)?.[1] || new Date().toISOString(),
        content: line.replace(/^\[.*?\]\s*/, ''),
        stream: 'stdout',
      }));
      setLogs(lines);
    } catch (err) {
      // Ignore log load errors
    }
  };

  const loadStatus = async () => {
    try {
      const res = await getTaskStatus(id);
      setStatus(res.data);
    } catch (err) {
      console.error('Failed to load status');
    }
  };

  const handleStart = async () => {
    try {
      await startTask(id);
      toast.success('任务已启动');
      loadStatus();
    } catch (err) {
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async () => {
    try {
      await stopTask(id);
      toast.success('任务已停止');
      loadStatus();
    } catch (err) {
      toast.error('停止失败');
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

  const handleRefreshLogs = () => {
    loadLogs();
    toast.success('日志已刷新');
  };

  if (loading || !task) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
        <div className="flex items-center gap-4">
          <button 
            onClick={() => navigate('/tasks')}
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
              {task.source_dir} → {task.remote_name}:{task.remote_dir}
            </p>
          </div>
        </div>

        <div className="flex items-center gap-2">
          {status.running ? (
            <button
              onClick={handleStop}
              className="inline-flex items-center gap-2 px-4 py-2 bg-red-50 text-red-600 rounded-lg hover:bg-red-100 transition-colors font-medium"
            >
              <Square className="w-4 h-4" />
              停止
            </button>
          ) : (
            <button
              onClick={handleStart}
              className="inline-flex items-center gap-2 px-4 py-2 bg-green-50 text-green-600 rounded-lg hover:bg-green-100 transition-colors font-medium"
            >
              <Play className="w-4 h-4" />
              启动
            </button>
          )}
          <button
            onClick={handleDedupe}
            className="inline-flex items-center gap-2 px-4 py-2 bg-purple-50 text-purple-600 rounded-lg hover:bg-purple-100 transition-colors font-medium"
          >
            <RotateCcw className="w-4 h-4" />
            去重
          </button>
          <Link
            to={`/tasks/${id}/edit`}
            className="inline-flex items-center gap-2 px-4 py-2 bg-blue-50 text-blue-600 rounded-lg hover:bg-blue-100 transition-colors font-medium"
          >
            <Pencil className="w-4 h-4" />
            编辑
          </Link>
          <button
            onClick={handleDelete}
            className="inline-flex items-center gap-2 px-4 py-2 bg-gray-50 text-gray-600 rounded-lg hover:bg-red-50 hover:text-red-600 transition-colors font-medium"
          >
            <Trash2 className="w-4 h-4" />
            删除
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
          value={task.watch_enabled ? '监控' : '手动'} 
          sub={task.schedule_enabled ? `定时 ${task.schedule_interval}分` : '无定时'}
        />
      </div>

      {/* Log Viewer */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
        <div className="px-6 py-4 border-b border-gray-100 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <Terminal className="w-5 h-5 text-gray-600" />
            <h2 className="font-semibold text-gray-900">实时日志</h2>
            {status.running && (
              <span className="px-2 py-0.5 bg-green-100 text-green-700 text-xs font-medium rounded-full animate-pulse">
                实时接收中
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <label className="flex items-center gap-2 text-sm text-gray-600 cursor-pointer">
              <input
                type="checkbox"
                checked={autoScroll}
                onChange={(e) => setAutoScroll(e.target.checked)}
                className="rounded border-gray-300 text-blue-600 focus:ring-blue-500"
              />
              自动滚动
            </label>
            <button
              onClick={handleRefreshLogs}
              className="p-1.5 text-gray-500 hover:bg-gray-100 rounded-lg transition-colors"
              title="刷新日志"
            >
              <RotateCcw className="w-4 h-4" />
            </button>
          </div>
        </div>

        <div className="log-viewer h-96 overflow-auto p-2">
          {logs.length === 0 ? (
            <div className="flex items-center justify-center h-full text-gray-500">
              <div className="text-center">
                <Terminal className="w-8 h-8 mx-auto mb-2 opacity-50" />
                <p>暂无日志</p>
                <p className="text-sm mt-1">启动任务后将显示实时输出</p>
              </div>
            </div>
          ) : (
            <div className="space-y-0">
              {logs.map((log, idx) => (
                <div 
                  key={idx} 
                  className={`log-line ${
                    log.content.includes('ERROR') ? 'log-error' :
                    log.content.includes('WARN') ? 'log-warn' :
                    log.content.includes('Transferred') ? 'log-success' :
                    'log-info'
                  }`}
                >
                  <span className="text-gray-500 mr-2">[{log.time}]</span>
                  {log.content}
                </div>
              ))}
              <div ref={logsEndRef} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

const StatusBadge = ({ status }) => {
  const configs = {
    running: { text: '运行中', class: 'bg-green-100 text-green-700' },
    idle: { text: '待机', class: 'bg-gray-100 text-gray-600' },
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

export default TaskDetail;

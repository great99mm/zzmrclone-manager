import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { 
  Play, 
  Square, 
  HardDrive, 
  Clock, 
  Activity,
  ArrowRight,
  CheckCircle2,
  AlertCircle
} from 'lucide-react';
import { getTasks, getSystemStats, startTask, stopTask } from '../services/api';
import { createWebSocket } from '../services/api';
import toast from 'react-hot-toast';

const Dashboard = () => {
  const [tasks, setTasks] = useState([]);
  const [stats, setStats] = useState({ total_tasks: 0, running_tasks: 0 });
  const [loading, setLoading] = useState(true);
  const [wsConnected, setWsConnected] = useState(false);

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
      const data = JSON.parse(event.data);
      if (data.type === 'task_complete' || data.type === 'task_error' ||
          data.type === 'task_started' || data.type === 'task_stopped') {
        loadData();
      }
    };

    // Refresh every 2 seconds (was 5s — too slow to feel real-time)
    const interval = setInterval(loadData, 2000);

    return () => {
      ws.close();
      clearInterval(interval);
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

  const handleStart = async (id) => {
    try {
      await startTask(id);
      toast.success('任务已启动');
      setTimeout(loadData, 300);
    } catch (err) {
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async (id) => {
    try {
      await stopTask(id);
      toast.success('任务已停止');
      setTimeout(loadData, 300);
    } catch (err) {
      toast.error('停止失败');
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

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">总览</h1>
          <p className="text-gray-500 mt-1">Rclone 自动化任务管理面板</p>
        </div>
        <div className="flex items-center gap-2">
          <span className={`w-2.5 h-2.5 rounded-full ${wsConnected ? 'bg-green-500' : 'bg-red-500'}`}></span>
          <span className="text-sm text-gray-500">{wsConnected ? '实时连接' : '离线'}</span>
        </div>
      </div>

      {/* Stats Cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
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
          <div className="px-6 py-4 border-b border-gray-100">
            <h2 className="font-semibold text-gray-900 flex items-center gap-2">
              <Play className="w-5 h-5 text-green-500" />
              正在运行 ({runningTasks.length})
            </h2>
          </div>
          <div className="divide-y divide-gray-100">
            {runningTasks.map(task => (
              <div key={task.id} className="px-6 py-4 flex items-center justify-between hover:bg-gray-50">
                <div>
                  <div className="font-medium text-gray-900">{task.name}</div>
                  <div className="text-sm text-gray-500 mt-0.5">
                    {task.source_dir} → {task.remote_name}:{task.remote_dir}
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  <span className="px-2.5 py-1 bg-green-100 text-green-700 text-xs font-medium rounded-full">
                    运行中
                  </span>
                  <Link 
                    to={`/tasks/${task.id}`}
                    className="text-blue-600 hover:text-blue-700 text-sm font-medium"
                  >
                    查看
                  </Link>
                  <button
                    onClick={() => handleStop(task.id)}
                    className="p-1.5 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                  >
                    <Square className="w-4 h-4" />
                  </button>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Task List */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200">
        <div className="px-6 py-4 border-b border-gray-100 flex items-center justify-between">
          <h2 className="font-semibold text-gray-900">任务列表</h2>
          <Link 
            to="/tasks/new"
            className="text-sm text-blue-600 hover:text-blue-700 font-medium"
          >
            + 新建任务
          </Link>
        </div>

        {tasks.length === 0 ? (
          <div className="px-6 py-12 text-center">
            <HardDrive className="w-12 h-12 text-gray-300 mx-auto mb-3" />
            <p className="text-gray-500">暂无任务，点击上方按钮创建</p>
          </div>
        ) : (
          <div className="divide-y divide-gray-100">
            {tasks.map(task => (
              <div key={task.id} className="px-6 py-4 flex items-center justify-between hover:bg-gray-50">
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-2">
                    <span className={`w-2 h-2 rounded-full ${
                      task.status === 'running' ? 'bg-green-500' : 
                      task.status === 'error' ? 'bg-red-500' : 'bg-gray-400'
                    }`}></span>
                    <span className="font-medium text-gray-900">{task.name}</span>
                  </div>
                  <div className="text-sm text-gray-500 mt-0.5 truncate">
                    {task.source_dir} → {task.remote_name}:{task.remote_dir}
                  </div>
                  <div className="flex items-center gap-3 mt-1.5 text-xs text-gray-400">
                    <span>并发: {task.transfers}</span>
                    <span>检查: {task.checkers}</span>
                    {task.watch_enabled && <span className="text-blue-500">目录监控</span>}
                    {task.schedule_enabled && <span className="text-purple-500">定时执行</span>}
                  </div>
                </div>

                <div className="flex items-center gap-2 ml-4">
                  {task.status !== 'running' ? (
                    <button
                      onClick={() => handleStart(task.id)}
                      className="p-2 text-green-600 hover:bg-green-50 rounded-lg transition-colors"
                      title="启动"
                    >
                      <Play className="w-4 h-4" />
                    </button>
                  ) : (
                    <button
                      onClick={() => handleStop(task.id)}
                      className="p-2 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                      title="停止"
                    >
                      <Square className="w-4 h-4" />
                    </button>
                  )}
                  <Link 
                    to={`/tasks/${task.id}`}
                    className="p-2 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors"
                    title="详情"
                  >
                    <ArrowRight className="w-4 h-4" />
                  </Link>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
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
    <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-5">
      <div className="flex items-center justify-between">
        <div>
          <p className="text-sm font-medium text-gray-500">{label}</p>
          <p className="text-2xl font-bold text-gray-900 mt-1">{value}</p>
        </div>
        <div className={`p-3 rounded-lg ${colors[color]}`}>
          <Icon className="w-6 h-6" />
        </div>
      </div>
    </div>
  );
};

export default Dashboard;

import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import {
  Plus,
  Play,
  Square,
  Pencil,
  Trash2,
  Search,
  Filter,
  Eye,
  CheckCircle2,
  XCircle,
  Clock
} from 'lucide-react';
import { getTasks, deleteTask, startTask, stopTask } from '../services/api';
import toast from 'react-hot-toast';

const Tasks = () => {
  const [tasks, setTasks] = useState([]);
  const [loading, setLoading] = useState(true);
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState('all');

  useEffect(() => {
    loadTasks();
    const interval = setInterval(loadTasks, 5000);
    return () => clearInterval(interval);
  }, []);

  const loadTasks = async () => {
    try {
      const res = await getTasks();
      setTasks(res.data);
    } catch (err) {
      toast.error('加载任务失败');
    } finally {
      setLoading(false);
    }
  };

  const handleDelete = async (id) => {
    if (!window.confirm('确定要删除这个任务吗？此操作不可恢复。')) return;

    try {
      await deleteTask(id);
      toast.success('任务已删除');
      loadTasks();
    } catch (err) {
      toast.error('删除失败');
    }
  };

  const handleStart = async (id) => {
    try {
      await startTask(id);
      toast.success('任务已启动');
      loadTasks();
    } catch (err) {
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async (id) => {
    try {
      await stopTask(id);
      toast.success('任务已停止');
      loadTasks();
    } catch (err) {
      toast.error('停止失败');
    }
  };

  const filteredTasks = tasks.filter(task => {
    const matchesSearch = task.name.toLowerCase().includes(search.toLowerCase()) ||
                         task.source_dir.toLowerCase().includes(search.toLowerCase()) ||
                         task.remote_name.toLowerCase().includes(search.toLowerCase());
    const matchesFilter = filter === 'all' || task.status === filter;
    return matchesSearch && matchesFilter;
  });

  if (loading) {
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
        <div>
          <h1 className="text-2xl font-bold text-gray-900">任务管理</h1>
          <p className="text-gray-500 mt-1">管理所有 Rclone 自动化任务</p>
        </div>
        <Link
          to="/tasks/new"
          className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium"
        >
          <Plus className="w-4 h-4" />
          新建任务
        </Link>
      </div>

      {/* Filters */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-4">
        <div className="flex flex-col md:flex-row gap-4">
          <div className="flex-1 relative">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
            <input
              type="text"
              placeholder="搜索任务名称、目录或盘符..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full pl-10 pr-4 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
            />
          </div>
          <div className="flex items-center gap-2">
            <Filter className="w-4 h-4 text-gray-400" />
            <select
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              className="px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
            >
              <option value="all">全部状态</option>
              <option value="idle">待机</option>
              <option value="running">运行中</option>
              <option value="error">异常</option>
            </select>
          </div>
        </div>
      </div>

      {/* Task Table */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full">
            <thead className="bg-gray-50 border-b border-gray-200">
              <tr>
                <th className="px-6 py-3 text-left text-xs font-semibold text-gray-500 uppercase tracking-wider">任务</th>
                <th className="px-6 py-3 text-left text-xs font-semibold text-gray-500 uppercase tracking-wider">路径</th>
                <th className="px-6 py-3 text-left text-xs font-semibold text-gray-500 uppercase tracking-wider">配置</th>
                <th className="px-6 py-3 text-left text-xs font-semibold text-gray-500 uppercase tracking-wider">状态</th>
                <th className="px-6 py-3 text-left text-xs font-semibold text-gray-500 uppercase tracking-wider">自动化</th>
                <th className="px-6 py-3 text-right text-xs font-semibold text-gray-500 uppercase tracking-wider">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {filteredTasks.length === 0 ? (
                <tr>
                  <td colSpan="6" className="px-6 py-12 text-center text-gray-500">
                    没有找到匹配的任务
                  </td>
                </tr>
              ) : (
                filteredTasks.map(task => (
                  <tr key={task.id} className="hover:bg-gray-50 transition-colors">
                    <td className="px-6 py-4">
                      <div className="font-medium text-gray-900">{task.name}</div>
                      <div className="text-xs text-gray-500 mt-0.5">ID: {task.id}</div>
                    </td>
                    <td className="px-6 py-4">
                      <div className="text-sm text-gray-700">{task.source_dir}</div>
                      <div className="text-sm text-gray-500">→ {task.remote_name}:{task.remote_dir}</div>
                    </td>
                    <td className="px-6 py-4">
                      <div className="text-xs space-y-0.5 text-gray-500">
                        <div>并发: {task.transfers}</div>
                        <div>检查: {task.checkers}</div>
                        <div>块大小: {task.drive_chunk_size}</div>
                      </div>
                    </td>
                    <td className="px-6 py-4">
                      <StatusBadge status={task.status} />
                    </td>
                    <td className="px-6 py-4">
                      <div className="flex flex-wrap gap-1">
                        {task.watch_enabled && (
                          <span className="px-2 py-0.5 bg-blue-100 text-blue-700 text-xs rounded-full">监控</span>
                        )}
                        {task.schedule_enabled && (
                          <span className="px-2 py-0.5 bg-purple-100 text-purple-700 text-xs rounded-full">定时</span>
                        )}
                        {task.auto_dedupe && (
                          <span className="px-2 py-0.5 bg-green-100 text-green-700 text-xs rounded-full">去重</span>
                        )}
                      </div>
                    </td>
                    <td className="px-6 py-4">
                      <div className="flex items-center justify-end gap-1">
                        {task.status !== 'running' ? (
                          <button
                            onClick={() => handleStart(task.id)}
                            className="p-1.5 text-green-600 hover:bg-green-50 rounded-lg transition-colors"
                            title="启动"
                          >
                            <Play className="w-4 h-4" />
                          </button>
                        ) : (
                          <button
                            onClick={() => handleStop(task.id)}
                            className="p-1.5 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                            title="停止"
                          >
                            <Square className="w-4 h-4" />
                          </button>
                        )}
                        <Link
                          to={`/tasks/${task.id}`}
                          className="p-1.5 text-gray-500 hover:bg-gray-100 rounded-lg transition-colors"
                          title="详情"
                        >
                          <Eye className="w-4 h-4" />
                        </Link>
                        <Link
                          to={`/tasks/${task.id}/edit`}
                          className="p-1.5 text-blue-500 hover:bg-blue-50 rounded-lg transition-colors"
                          title="编辑"
                        >
                          <Pencil className="w-4 h-4" />
                        </Link>
                        <button
                          onClick={() => handleDelete(task.id)}
                          className="p-1.5 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                          title="删除"
                        >
                          <Trash2 className="w-4 h-4" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
};

const StatusBadge = ({ status }) => {
  const configs = {
    running: { icon: Play, text: '运行中', class: 'bg-green-100 text-green-700' },
    idle: { icon: Clock, text: '待机', class: 'bg-gray-100 text-gray-600' },
    error: { icon: XCircle, text: '异常', class: 'bg-red-100 text-red-700' },
  };

  const config = configs[status] || configs.idle;
  const Icon = config.icon;

  return (
    <span className={`inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-medium ${config.class}`}>
      <Icon className="w-3.5 h-3.5" />
      {config.text}
    </span>
  );
};

export default Tasks;

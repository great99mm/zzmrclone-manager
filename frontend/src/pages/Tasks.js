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
  XCircle,
  Clock,
  FolderOpen,
  HardDrive
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
    setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'running' } : t));
    try {
      await startTask(id);
      toast.success('任务已启动');
      loadTasks();
    } catch (err) {
      setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'idle' } : t));
      toast.error(err.response?.data?.error || '启动失败');
    }
  };

  const handleStop = async (id) => {
    setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'idle' } : t));
    try {
      await stopTask(id);
      toast.success('任务已停止');
      loadTasks();
    } catch (err) {
      setTasks(prev => prev.map(t => t.id === id ? { ...t, status: 'running' } : t));
      toast.error('停止失败');
    }
  };

  const filteredTasks = tasks.filter(task => {
    const matchesSearch = task.name.toLowerCase().includes(search.toLowerCase()) ||
                         task.source_dir.toLowerCase().includes(search.toLowerCase()) ||
                         (task.remote_name || '').toLowerCase().includes(search.toLowerCase()) ||
                         (task.remote_dir || '').toLowerCase().includes(search.toLowerCase());
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
    <div className="space-y-4 md:space-y-6">
      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
        <div>
          <h1 className="text-xl md:text-2xl font-bold text-gray-900">任务管理</h1>
          <p className="text-sm md:text-base text-gray-500 mt-1">管理所有 Rclone 自动化任务</p>
        </div>
        <Link
          to="/tasks/new"
          className="inline-flex items-center justify-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium text-sm md:text-base"
        >
          <Plus className="w-4 h-4" />
          新建任务
        </Link>
      </div>

      {/* Filters */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-3 md:p-4">
        <div className="flex flex-col md:flex-row gap-3 md:gap-4">
          <div className="flex-1 relative">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
            <input
              type="text"
              placeholder="搜索任务名称、目录或盘符..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full pl-10 pr-4 py-2 text-sm border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
            />
          </div>
          <div className="flex items-center gap-1.5 flex-wrap">
            <Filter className="w-4 h-4 text-gray-400 flex-shrink-0" />
            {[
              { value: 'all', label: '全部' },
              { value: 'idle', label: '待机' },
              { value: 'running', label: '运行中' },
              { value: 'error', label: '异常' },
            ].map(({ value, label }) => (
              <button
                key={value}
                onClick={() => setFilter(value)}
                className={`px-3 py-1.5 text-xs md:text-sm rounded-lg font-medium transition-colors ${
                  filter === value
                    ? 'bg-blue-600 text-white'
                    : 'bg-gray-100 text-gray-600 hover:bg-gray-200'
                }`}
              >
                {label}
              </button>
            ))}
          </div>
        </div>
      </div>

      {/* Desktop: Task Table */}
      <div className="hidden md:block bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
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
                      <div className="text-sm text-gray-700">
                        {(task.source_type === 'remote' ? '☁ ' : '📂 ') + task.source_dir}
                      </div>
                      <div className="text-sm text-gray-500">→ {
                        task.dest_type === 'local'
                          ? '📂 ' + (task.remote_dir || '')
                          : '☁ ' + (task.remote_name || '') + ':' + (task.remote_dir || '')
                      }</div>
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
                        {task.interface_log_enabled && (
                          <span className="px-2 py-0.5 bg-cyan-100 text-cyan-700 text-xs rounded-full">接口日志</span>
                        )}
                        {task.schedule_enabled && (
                          <span className="px-2 py-0.5 bg-purple-100 text-purple-700 text-xs rounded-full">定时</span>
                        )}
                        {task.completion_action === 'smartstrm' && (
                          <span className="px-2 py-0.5 bg-orange-100 text-orange-700 text-xs rounded-full">SmartStrm</span>
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

      {/* Mobile: Task Cards */}
      <div className="md:hidden space-y-3">
        {filteredTasks.length === 0 ? (
          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-8 text-center text-gray-500">
            没有找到匹配的任务
          </div>
        ) : (
          filteredTasks.map(task => (
            <div key={task.id} className="bg-white rounded-xl shadow-sm border border-gray-200 p-4">
              {/* Card Header */}
              <div className="flex items-start justify-between mb-3">
                <div className="min-w-0 flex-1 mr-2">
                  <div className="flex items-center gap-2">
                    <h3 className="font-semibold text-gray-900 truncate">{task.name}</h3>
                    <StatusBadge status={task.status} />
                  </div>
                  <p className="text-xs text-gray-500 mt-0.5">ID: {task.id}</p>
                </div>
              </div>

              {/* Card Body: Paths */}
              <div className="bg-gray-50 rounded-lg p-3 space-y-1.5 mb-3">
                <div className="flex items-start gap-2 text-sm">
                  <FolderOpen className="w-3.5 h-3.5 text-gray-400 mt-0.5 flex-shrink-0" />
                  <span className="text-gray-700 break-all">
                    {(task.source_type === 'remote' ? '☁ ' : '') + task.source_dir}
                  </span>
                </div>
                <div className="flex items-start gap-2 text-sm">
                  <HardDrive className="w-3.5 h-3.5 text-gray-400 mt-0.5 flex-shrink-0" />
                  <span className="text-gray-700 break-all">{
                    task.dest_type === 'local'
                      ? task.remote_dir || ''
                      : (task.remote_name || '') + ':' + (task.remote_dir || '')
                  }</span>
                </div>
              </div>

              {/* Card Body: Config tags */}
              <div className="flex items-center gap-2 mb-3 text-xs text-gray-500">
                <span className="bg-gray-100 px-2 py-0.5 rounded">并发 {task.transfers}</span>
                <span className="bg-gray-100 px-2 py-0.5 rounded">检查 {task.checkers}</span>
                <span className="bg-gray-100 px-2 py-0.5 rounded">{task.drive_chunk_size}</span>
              </div>

              {/* Card Body: Automation tags */}
              <div className="flex flex-wrap gap-1 mb-3">
                {task.watch_enabled && (
                  <span className="px-2 py-0.5 bg-blue-100 text-blue-700 text-xs rounded-full">监控</span>
                )}
                {task.interface_log_enabled && (
                  <span className="px-2 py-0.5 bg-cyan-100 text-cyan-700 text-xs rounded-full">接口日志</span>
                )}
                {task.schedule_enabled && (
                  <span className="px-2 py-0.5 bg-purple-100 text-purple-700 text-xs rounded-full">定时</span>
                )}
                {task.completion_action === 'smartstrm' && (
                  <span className="px-2 py-0.5 bg-orange-100 text-orange-700 text-xs rounded-full">SmartStrm</span>
                )}
                {task.auto_dedupe && (
                  <span className="px-2 py-0.5 bg-green-100 text-green-700 text-xs rounded-full">去重</span>
                )}
              </div>

              {/* Card Footer: Actions */}
              <div className="flex items-center gap-2 pt-3 border-t border-gray-100">
                {task.status !== 'running' ? (
                  <button
                    onClick={() => handleStart(task.id)}
                    className="flex-1 inline-flex items-center justify-center gap-1.5 px-3 py-2 bg-green-50 text-green-600 rounded-lg hover:bg-green-100 transition-colors text-sm font-medium"
                  >
                    <Play className="w-3.5 h-3.5" />
                    启动
                  </button>
                ) : (
                  <button
                    onClick={() => handleStop(task.id)}
                    className="flex-1 inline-flex items-center justify-center gap-1.5 px-3 py-2 bg-red-50 text-red-600 rounded-lg hover:bg-red-100 transition-colors text-sm font-medium"
                  >
                    <Square className="w-3.5 h-3.5" />
                    停止
                  </button>
                )}
                <Link
                  to={`/tasks/${task.id}`}
                  className="flex-1 inline-flex items-center justify-center gap-1.5 px-3 py-2 bg-gray-50 text-gray-600 rounded-lg hover:bg-gray-100 transition-colors text-sm font-medium"
                >
                  <Eye className="w-3.5 h-3.5" />
                  详情
                </Link>
                <Link
                  to={`/tasks/${task.id}/edit`}
                  className="flex-1 inline-flex items-center justify-center gap-1.5 px-3 py-2 bg-blue-50 text-blue-600 rounded-lg hover:bg-blue-100 transition-colors text-sm font-medium"
                >
                  <Pencil className="w-3.5 h-3.5" />
                  编辑
                </Link>
                <button
                  onClick={() => handleDelete(task.id)}
                  className="inline-flex items-center justify-center p-2 text-red-500 hover:bg-red-50 rounded-lg transition-colors"
                  title="删除"
                >
                  <Trash2 className="w-4 h-4" />
                </button>
              </div>
            </div>
          ))
        )}
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

import React, { useState, useEffect } from 'react';
import { FileText, Trash2, RotateCcw, Search } from 'lucide-react';
import { getSystemLogs, cleanLogs, getTaskLogs } from '../services/api';
import toast from 'react-hot-toast';

const Logs = () => {
  const [activeTab, setActiveTab] = useState('system');
  const [logs, setLogs] = useState([]);
  const [tasks, setTasks] = useState([]);
  const [selectedTask, setSelectedTask] = useState('');
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (activeTab === 'system') {
      loadSystemLogs();
    } else {
      loadTaskList();
    }
  }, [activeTab]);

  const loadSystemLogs = async () => {
    setLoading(true);
    try {
      const res = await getSystemLogs('system.log', 500);
      const content = res.data.logs[0] || '';
      setLogs(content.split('\n').filter(l => l.trim()));
    } catch (err) {
      toast.error('加载系统日志失败');
    } finally {
      setLoading(false);
    }
  };

  const loadTaskList = async () => {
    try {
      const res = await fetch('/api/tasks').then(r => r.json());
      setTasks(res);
      if (res.length > 0 && !selectedTask) {
        setSelectedTask(res[0].id.toString());
      }
    } catch (err) {
      console.error('Failed to load tasks');
    }
  };

  const loadTaskLog = async (taskId) => {
    if (!taskId) return;
    setLoading(true);
    try {
      const res = await getTaskLogs(taskId, 500);
      const content = res.data.logs[0] || '';
      setLogs(content.split('\n').filter(l => l.trim()));
    } catch (err) {
      toast.error('加载任务日志失败');
    } finally {
      setLoading(false);
    }
  };

  const handleClean = async () => {
    if (!window.confirm('确定要清空所有日志吗？此操作不可恢复。')) return;
    try {
      await cleanLogs();
      toast.success('日志已清空');
      setLogs([]);
    } catch (err) {
      toast.error('清空失败');
    }
  };

  const handleRefresh = () => {
    if (activeTab === 'system') {
      loadSystemLogs();
    } else if (selectedTask) {
      loadTaskLog(selectedTask);
    }
    toast.success('已刷新');
  };

  const filteredLogs = logs.filter(line => 
    line.toLowerCase().includes(search.toLowerCase())
  );

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">日志查看</h1>
          <p className="text-gray-500 mt-1">查看系统及各任务的运行日志</p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={handleRefresh}
            className="inline-flex items-center gap-2 px-4 py-2 bg-white border border-gray-300 rounded-lg hover:bg-gray-50 transition-colors font-medium"
          >
            <RotateCcw className="w-4 h-4" />
            刷新
          </button>
          <button
            onClick={handleClean}
            className="inline-flex items-center gap-2 px-4 py-2 bg-red-50 text-red-600 border border-red-200 rounded-lg hover:bg-red-100 transition-colors font-medium"
          >
            <Trash2 className="w-4 h-4" />
            清空日志
          </button>
        </div>
      </div>

      {/* Tabs */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200">
        <div className="flex border-b border-gray-200">
          <button
            onClick={() => setActiveTab('system')}
            className={`px-6 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'system' 
                ? 'border-blue-600 text-blue-600' 
                : 'border-transparent text-gray-500 hover:text-gray-700'
            }`}
          >
            <div className="flex items-center gap-2">
              <FileText className="w-4 h-4" />
              系统日志
            </div>
          </button>
          <button
            onClick={() => setActiveTab('task')}
            className={`px-6 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'task' 
                ? 'border-blue-600 text-blue-600' 
                : 'border-transparent text-gray-500 hover:text-gray-700'
            }`}
          >
            <div className="flex items-center gap-2">
              <FileText className="w-4 h-4" />
              任务日志
            </div>
          </button>
        </div>

        <div className="p-4">
          {/* Task selector */}
          {activeTab === 'task' && (
            <div className="mb-4">
              <select
                value={selectedTask}
                onChange={(e) => {
                  setSelectedTask(e.target.value);
                  loadTaskLog(e.target.value);
                }}
                className="px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              >
                <option value="">选择任务</option>
                {tasks.map(task => (
                  <option key={task.id} value={task.id}>{task.name}</option>
                ))}
              </select>
            </div>
          )}

          {/* Search */}
          <div className="relative mb-4">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
            <input
              type="text"
              placeholder="搜索日志内容..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full pl-10 pr-4 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
            />
          </div>

          {/* Log content */}
          <div className="log-viewer h-[600px] overflow-auto rounded-lg">
            {loading ? (
              <div className="flex items-center justify-center h-full text-gray-500">
                <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-white"></div>
              </div>
            ) : filteredLogs.length === 0 ? (
              <div className="flex items-center justify-center h-full text-gray-500">
                <div className="text-center">
                  <FileText className="w-8 h-8 mx-auto mb-2 opacity-50" />
                  <p>暂无日志</p>
                </div>
              </div>
            ) : (
              <div className="space-y-0">
                {filteredLogs.map((line, idx) => (
                  <div 
                    key={idx} 
                    className={`log-line ${
                      line.includes('ERROR') ? 'log-error' :
                      line.includes('WARN') ? 'log-warn' :
                      line.includes('Transferred') || line.includes('success') ? 'log-success' :
                      'log-info'
                    }`}
                  >
                    {line}
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

export default Logs;

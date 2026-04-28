import React, { useState, useEffect } from 'react';
import { FileText, Trash2, RotateCcw, Search, List, ChevronLeft, ChevronRight, LayoutList, Terminal } from 'lucide-react';
import { getSystemLogs, cleanLogs, getTaskLogs, getOutputLogs, deleteOutputLog, cleanOutputLogs } from '../services/api';
import toast from 'react-hot-toast';

const Logs = () => {
  const [activeTab, setActiveTab] = useState('records');
  const [logs, setLogs] = useState([]);
  const [tasks, setTasks] = useState([]);
  const [selectedTask, setSelectedTask] = useState('');
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(false);

  // Output logs (persistent structured transfer records)
  const [outputLogs, setOutputLogs] = useState([]);
  const [outputLogsTotal, setOutputLogsTotal] = useState(0);
  const [outputLogsPage, setOutputLogsPage] = useState(1);
  const [failedTasksCount, setFailedTasksCount] = useState(0);

  useEffect(() => {
    loadTaskList();
  }, []);

  useEffect(() => {
    if (activeTab === 'system') {
      loadSystemLogs();
    } else if (activeTab === 'task') {
      if (selectedTask) {
        loadTaskLog(selectedTask);
      }
    } else if (activeTab === 'records') {
      loadOutputLogs();
    }
  }, [activeTab, outputLogsPage, selectedTask]);

  const loadTaskList = async () => {
    try {
      const res = await fetch('/api/tasks').then(r => r.json());
      setTasks(res);
      // Count tasks with error status
      const failed = res.filter(t => t.status === 'error').length;
      setFailedTasksCount(failed);
    } catch (err) {
      console.error('Failed to load tasks');
    }
  };

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

  const loadOutputLogs = async () => {
    setLoading(true);
    try {
      const res = await getOutputLogs(outputLogsPage, 20, selectedTask);
      if (res.data && res.data.success) {
        const list = res.data.data.list || [];
        setOutputLogs(list);
        setOutputLogsTotal(res.data.data.total || 0);
      } else {
        setOutputLogs([]);
        setOutputLogsTotal(0);
      }
    } catch (err) {
      toast.error('加载转移记录失败');
      setOutputLogs([]);
      setOutputLogsTotal(0);
    } finally {
      setLoading(false);
    }
  };

  const handleDeleteOutputLog = async (id) => {
    if (!window.confirm('确定删除这条记录吗？')) return;
    try {
      await deleteOutputLog(id);
      toast.success('记录已删除');
      loadOutputLogs();
    } catch (err) {
      toast.error('删除失败');
    }
  };

  const handleCleanOutputLogs = async () => {
    if (!window.confirm('确定要清空所有转移记录吗？此操作不可恢复。')) return;
    try {
      await cleanOutputLogs(selectedTask);
      toast.success('记录已清空');
      setOutputLogs([]);
      setOutputLogsTotal(0);
    } catch (err) {
      toast.error('清空失败');
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
    } else if (activeTab === 'task') {
      if (selectedTask) {
        loadTaskLog(selectedTask);
      }
    } else if (activeTab === 'records') {
      loadOutputLogs();
      loadTaskList();
    }
    toast.success('已刷新');
  };

  const filteredLogs = logs.filter(line => 
    line.toLowerCase().includes(search.toLowerCase())
  );

  const formatFileSize = (bytes) => {
    if (!bytes || bytes <= 0) return '-';
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(2) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(2) + ' MB';
    return (bytes / 1024 / 1024 / 1024).toFixed(2) + ' GB';
  };

  const formatDate = (dateStr) => {
    if (!dateStr) return '-';
    const d = new Date(dateStr);
    if (isNaN(d.getTime())) return dateStr;
    const yyyy = d.getFullYear();
    const mm = String(d.getMonth() + 1).padStart(2, '0');
    const dd = String(d.getDate()).padStart(2, '0');
    const hh = String(d.getHours()).padStart(2, '0');
    const mi = String(d.getMinutes()).padStart(2, '0');
    const ss = String(d.getSeconds()).padStart(2, '0');
    return `${yyyy}-${mm}-${dd}\n${hh}:${mi}:${ss}`;
  };

  const taskCount = tasks.length;

  return (
    <div className="space-y-6">
      {/* Stats Cards */}
      {activeTab === 'records' && (
        <div className="grid grid-cols-2 gap-4">
          <div className="bg-gray-800 rounded-xl p-5 border border-gray-700">
            <div className="flex items-center gap-3 mb-3">
              <div className="w-10 h-10 rounded-full bg-red-500/20 flex items-center justify-center">
                <span className="text-red-400 text-lg font-bold">×</span>
              </div>
            </div>
            <div className="text-3xl font-bold text-red-400">{failedTasksCount}</div>
            <div className="text-gray-400 text-sm mt-1">任务失败</div>
          </div>
          <div className="bg-gray-800 rounded-xl p-5 border border-gray-700">
            <div className="flex items-center gap-3 mb-3">
              <div className="w-10 h-10 rounded-full bg-blue-500/20 flex items-center justify-center">
                <LayoutList className="w-5 h-5 text-blue-400" />
              </div>
            </div>
            <div className="text-3xl font-bold text-blue-400">{taskCount}</div>
            <div className="text-gray-400 text-sm mt-1">任务数</div>
          </div>
        </div>
      )}

      {/* Tabs */}
      <div className="bg-gray-800 rounded-xl shadow-sm border border-gray-700">
        <div className="flex border-b border-gray-700">
          <button
            onClick={() => setActiveTab('records')}
            className={`px-6 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'records' 
                ? 'border-blue-500 text-blue-400' 
                : 'border-transparent text-gray-400 hover:text-gray-200'
            }`}
          >
            <div className="flex items-center gap-2">
              <LayoutList className="w-4 h-4" />
              记录
            </div>
          </button>
          <button
            onClick={() => setActiveTab('task')}
            className={`px-6 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'task' 
                ? 'border-blue-500 text-blue-400' 
                : 'border-transparent text-gray-400 hover:text-gray-200'
            }`}
          >
            <div className="flex items-center gap-2">
              <FileText className="w-4 h-4" />
              任务
            </div>
          </button>
          <button
            onClick={() => setActiveTab('system')}
            className={`px-6 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'system' 
                ? 'border-blue-500 text-blue-400' 
                : 'border-transparent text-gray-400 hover:text-gray-200'
            }`}
          >
            <div className="flex items-center gap-2">
              <Terminal className="w-4 h-4" />
              日志
            </div>
          </button>
        </div>

        <div className="p-4">
          {/* Header + Controls */}
          <div className="flex flex-col md:flex-row md:items-center justify-between gap-4 mb-4">
            {activeTab === 'records' ? (
              <>
                <div className="flex items-center gap-3">
                  <List className="w-5 h-5 text-blue-400" />
                  <h2 className="text-lg font-semibold text-gray-100">转移记录</h2>
                </div>
                <div className="flex items-center gap-2">
                  <select
                    value={selectedTask}
                    onChange={(e) => {
                      setSelectedTask(e.target.value);
                      setOutputLogsPage(1);
                    }}
                    className="px-3 py-2 bg-gray-700 border border-gray-600 text-gray-100 rounded-lg focus:ring-2 focus:ring-blue-500 text-sm"
                  >
                    <option value="">全部任务</option>
                    {tasks.map(task => (
                      <option key={task.id} value={task.id}>{task.name}</option>
                    ))}
                  </select>
                  <button
                    onClick={handleRefresh}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-gray-700 border border-gray-600 text-gray-200 rounded-lg hover:bg-gray-600 transition-colors text-sm"
                  >
                    <RotateCcw className="w-3.5 h-3.5" />
                    刷新
                  </button>
                  <button
                    onClick={handleCleanOutputLogs}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-red-500/20 text-red-400 border border-red-500/30 rounded-lg hover:bg-red-500/30 transition-colors text-sm"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                    清空
                  </button>
                </div>
              </>
            ) : (
              <>
                <div className="flex items-center gap-3">
                  {activeTab === 'system' ? (
                    <>
                      <Terminal className="w-5 h-5 text-blue-400" />
                      <h2 className="text-lg font-semibold text-gray-100">系统日志</h2>
                    </>
                  ) : (
                    <>
                      <FileText className="w-5 h-5 text-blue-400" />
                      <h2 className="text-lg font-semibold text-gray-100">任务日志</h2>
                    </>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  {activeTab === 'task' && (
                    <select
                      value={selectedTask}
                      onChange={(e) => {
                        setSelectedTask(e.target.value);
                        loadTaskLog(e.target.value);
                      }}
                      className="px-3 py-2 bg-gray-700 border border-gray-600 text-gray-100 rounded-lg focus:ring-2 focus:ring-blue-500 text-sm"
                    >
                      <option value="">选择任务</option>
                      {tasks.map(task => (
                        <option key={task.id} value={task.id}>{task.name}</option>
                      ))}
                    </select>
                  )}
                  <button
                    onClick={handleRefresh}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-gray-700 border border-gray-600 text-gray-200 rounded-lg hover:bg-gray-600 transition-colors text-sm"
                  >
                    <RotateCcw className="w-3.5 h-3.5" />
                    刷新
                  </button>
                  <button
                    onClick={handleClean}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-red-500/20 text-red-400 border border-red-500/30 rounded-lg hover:bg-red-500/30 transition-colors text-sm"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                    清空日志
                  </button>
                </div>
              </>
            )}
          </div>

          {/* Content */}
          {activeTab === 'records' ? (
            /* Structured Output Logs - Table */
            <div className="overflow-x-auto">
              <table className="w-full text-sm text-left">
                <thead className="text-xs text-gray-400 uppercase bg-gray-700/50">
                  <tr>
                    <th className="px-4 py-3 rounded-tl-lg w-16">状态</th>
                    <th className="px-4 py-3">文件</th>
                    <th className="px-4 py-3">源路径</th>
                    <th className="px-4 py-3">目标路径</th>
                    <th className="px-4 py-3">大小</th>
                    <th className="px-4 py-3 w-28">时间</th>
                    <th className="px-4 py-3 rounded-tr-lg w-16">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {loading ? (
                    <tr>
                      <td colSpan="7" className="px-4 py-8 text-center text-gray-500">
                        <div className="flex items-center justify-center">
                          <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-blue-400"></div>
                        </div>
                      </td>
                    </tr>
                  ) : outputLogs.length === 0 ? (
                    <tr>
                      <td colSpan="7" className="px-4 py-8 text-center text-gray-500">
                        <List className="w-8 h-8 mx-auto mb-2 opacity-50" />
                        <p>暂无转移记录</p>
                        <p className="text-xs mt-1">执行任务后，文件传输记录将显示在这里</p>
                      </td>
                    </tr>
                  ) : (
                    outputLogs.map((log, idx) => (
                      <tr 
                        key={log.id} 
                        className={`border-b border-gray-700/50 hover:bg-gray-700/30 transition-colors ${
                          idx === outputLogs.length - 1 ? 'border-b-0' : ''
                        }`}
                      >
                        <td className="px-4 py-3">
                          {log.status ? (
                            <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-green-500/20 text-green-400">
                              成功
                            </span>
                          ) : (
                            <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-red-500/20 text-red-400">
                              失败
                            </span>
                          )}
                        </td>
                        <td className="px-4 py-3">
                          <div className="text-gray-200 font-medium max-w-[200px] truncate" title={log.file_name}>
                            {log.file_name || '-'}
                          </div>
                          {log.file_ext && (
                            <span className="text-xs text-gray-500">{log.file_ext}</span>
                          )}
                        </td>
                        <td className="px-4 py-3">
                          <div className="text-gray-300 max-w-[200px] truncate" title={log.src}>
                            {log.src || '-'}
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <div className="text-gray-300 max-w-[200px] truncate" title={log.dest}>
                            {log.dest || '-'}
                          </div>
                        </td>
                        <td className="px-4 py-3 text-gray-300 whitespace-nowrap">
                          {formatFileSize(log.file_size)}
                        </td>
                        <td className="px-4 py-3 text-gray-400 whitespace-pre-line text-xs">
                          {formatDate(log.date)}
                        </td>
                        <td className="px-4 py-3">
                          <button
                            onClick={() => handleDeleteOutputLog(log.id)}
                            className="text-gray-500 hover:text-red-400 transition-colors"
                            title="删除"
                          >
                            <Trash2 className="w-4 h-4" />
                          </button>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>

              {/* Pagination */}
              {!loading && outputLogsTotal > 0 && (
                <div className="flex items-center justify-between px-4 py-3 border-t border-gray-700/50 mt-2">
                  <div className="text-sm text-gray-400">
                    共 {outputLogsTotal} 条记录
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => setOutputLogsPage(p => Math.max(1, p - 1))}
                      disabled={outputLogsPage <= 1}
                      className="p-2 bg-gray-700 border border-gray-600 text-gray-300 rounded-lg hover:bg-gray-600 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                    >
                      <ChevronLeft className="w-4 h-4" />
                    </button>
                    <span className="text-sm text-gray-300 px-2">
                      第 {outputLogsPage} 页
                      {outputLogsTotal > 0 && `（${Math.min(outputLogsTotal, 20)} 条）`}
                    </span>
                    <button
                      onClick={() => setOutputLogsPage(p => p + 1)}
                      disabled={outputLogsPage * 20 >= outputLogsTotal}
                      className="p-2 bg-gray-700 border border-gray-600 text-gray-300 rounded-lg hover:bg-gray-600 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                    >
                      <ChevronRight className="w-4 h-4" />
                    </button>
                    <button
                      onClick={handleRefresh}
                      className="inline-flex items-center gap-1 px-3 py-2 bg-gray-700 border border-gray-600 text-gray-300 rounded-lg hover:bg-gray-600 transition-colors text-sm ml-2"
                    >
                      <RotateCcw className="w-3.5 h-3.5" />
                      刷新
                    </button>
                  </div>
                </div>
              )}
            </div>
          ) : (
            /* Raw log viewer (system / task tabs) */
            <>
              {/* Search */}
              <div className="relative mb-4">
                <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-500" />
                <input
                  type="text"
                  placeholder="搜索日志内容..."
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  className="w-full pl-10 pr-4 py-2 bg-gray-700 border border-gray-600 text-gray-100 rounded-lg focus:ring-2 focus:ring-blue-500 placeholder-gray-500"
                />
              </div>

              {/* Log content */}
              <div className="bg-gray-900 rounded-lg h-[600px] overflow-auto font-mono text-sm">
                {loading ? (
                  <div className="flex items-center justify-center h-full text-gray-500">
                    <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-400"></div>
                  </div>
                ) : filteredLogs.length === 0 ? (
                  <div className="flex items-center justify-center h-full text-gray-500">
                    <div className="text-center">
                      <FileText className="w-8 h-8 mx-auto mb-2 opacity-50" />
                      <p>暂无日志</p>
                    </div>
                  </div>
                ) : (
                  <div className="divide-y divide-gray-800">
                    {filteredLogs.map((line, idx) => (
                      <div 
                        key={idx} 
                        className={`px-4 py-2 ${
                          line.includes('ERROR') ? 'text-red-400 bg-red-500/5' :
                          line.includes('WARN') ? 'text-yellow-400 bg-yellow-500/5' :
                          line.includes('Transferred') || line.includes('success') ? 'text-green-400 bg-green-500/5' :
                          'text-gray-300'
                        }`}
                      >
                        {line}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
};

export default Logs;

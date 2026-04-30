import React, { useState, useEffect, useRef } from 'react';
import { FileText, Trash2, RotateCcw, Search, List, ChevronLeft, ChevronRight, LayoutList, HardDrive, CheckCircle2 } from 'lucide-react';
import { getTaskLogs, getOutputLogs, deleteOutputLog, cleanOutputLogs } from '../services/api';
import { createWebSocket } from '../services/api';
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

  // Real-time transfer progress from WebSocket
  const [transferProgress, setTransferProgress] = useState({});
  const wsRef = useRef(null);

  useEffect(() => {
    loadTaskList();
  }, []);

  useEffect(() => {
    if (activeTab === 'task') {
      if (selectedTask) {
        loadTaskLog(selectedTask);
      }
    } else if (activeTab === 'records') {
      loadOutputLogs();
    }
  }, [activeTab, outputLogsPage, selectedTask]);

  // WebSocket for real-time file progress
  useEffect(() => {
    const ws = createWebSocket();
    wsRef.current = ws;

    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        if (data.type === 'file_progress') {
          setTransferProgress(prev => ({
            ...prev,
            [`${data.task_id}_${data.file_name}`]: data
          }));
        } else if (data.type === 'task_complete' || data.type === 'task_error') {
          // Clear progress for completed tasks
          setTransferProgress(prev => {
            const next = { ...prev };
            Object.keys(next).forEach(k => {
              if (k.startsWith(`${data.task_id}_`)) delete next[k];
            });
            return next;
          });
          if (activeTab === 'records') {
            loadOutputLogs();
          }
        }
      } catch (e) {
        // ignore
      }
    };

    return () => {
      ws.close();
    };
  }, [activeTab]);

  const loadTaskList = async () => {
    try {
      const res = await fetch('/api/tasks').then(r => r.json());
      setTasks(res);
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

  const handleRefresh = () => {
    if (activeTab === 'task') {
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

  // Merge real-time progress with completed logs
  const activeTransfers = Object.values(transferProgress).filter(
    t => !selectedTask || String(t.task_id) === selectedTask
  );

  const ProgressBar = ({ progress, bytes, size, speed }) => (
    <div className="w-full">
      <div className="flex items-center justify-between text-xs mb-1">
        <span className={`font-medium ${progress >= 100 ? 'text-green-600' : 'text-blue-600'}`}>
          {progress >= 100 ? '已完成' : `${Math.round(progress)}%`}
        </span>
        {speed > 0 && progress < 100 && (
          <span className="text-gray-400">{formatFileSize(speed)}/s</span>
        )}
      </div>
      <div className="w-full bg-gray-200 rounded-full h-2 overflow-hidden">
        <div
          className={`h-full rounded-full transition-all duration-500 ${
            progress >= 100 ? 'bg-green-500' : 'bg-blue-500'
          }`}
          style={{ width: `${Math.min(100, progress)}%` }}
        />
      </div>
      {bytes > 0 && size > 0 && progress < 100 && (
        <div className="text-xs text-gray-400 mt-0.5">
          {formatFileSize(bytes)} / {formatFileSize(size)}
        </div>
      )}
    </div>
  );

  return (
    <div className="space-y-4 md:space-y-6">
      {/* Stats Cards */}
      {activeTab === 'records' && (
        <div className="grid grid-cols-1 md:grid-cols-2 gap-3 md:gap-4">
          <div className="bg-white rounded-xl p-4 md:p-5 border border-gray-200 shadow-sm">
            <div className="flex items-center gap-3 mb-2 md:mb-3">
              <div className="w-9 h-9 md:w-10 md:h-10 rounded-full bg-blue-50 flex items-center justify-center">
                <LayoutList className="w-4 h-4 md:w-5 md:h-5 text-blue-500" />
              </div>
              <div>
                <div className="text-2xl md:text-3xl font-bold text-gray-900">{taskCount}</div>
                <div className="text-gray-500 text-xs md:text-sm">任务数</div>
              </div>
            </div>
          </div>
          <div className="bg-white rounded-xl p-4 md:p-5 border border-gray-200 shadow-sm">
            <div className="flex items-center gap-3 mb-2 md:mb-3">
              <div className="w-9 h-9 md:w-10 md:h-10 rounded-full bg-green-50 flex items-center justify-center">
                <CheckCircle2 className="w-4 h-4 md:w-5 md:h-5 text-green-500" />
              </div>
              <div>
                <div className="text-2xl md:text-3xl font-bold text-gray-900">{outputLogsTotal}</div>
                <div className="text-gray-500 text-xs md:text-sm">已完成记录</div>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Tabs */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200">
        <div className="flex border-b border-gray-200 overflow-x-auto">
          <button
            onClick={() => setActiveTab('records')}
            className={`px-4 md:px-6 py-3 text-sm font-medium border-b-2 transition-colors whitespace-nowrap flex-shrink-0 ${
              activeTab === 'records'
                ? 'border-blue-500 text-blue-600'
                : 'border-transparent text-gray-500 hover:text-gray-700'
            }`}
          >
            <div className="flex items-center gap-2">
              <LayoutList className="w-4 h-4" />
              转移记录
            </div>
          </button>
          <button
            onClick={() => setActiveTab('task')}
            className={`px-4 md:px-6 py-3 text-sm font-medium border-b-2 transition-colors whitespace-nowrap flex-shrink-0 ${
              activeTab === 'task'
                ? 'border-blue-500 text-blue-600'
                : 'border-transparent text-gray-500 hover:text-gray-700'
            }`}
          >
            <div className="flex items-center gap-2">
              <FileText className="w-4 h-4" />
              任务日志
            </div>
          </button>
        </div>

        <div className="p-3 md:p-4">
          {/* Header + Controls */}
          <div className="flex flex-col gap-3 md:flex-row md:items-center justify-between mb-4">
            {activeTab === 'records' ? (
              <>
                <div className="flex items-center gap-2 md:gap-3">
                  <List className="w-5 h-5 text-blue-500" />
                  <h2 className="text-base md:text-lg font-semibold text-gray-900">转移记录</h2>
                  {activeTransfers.length > 0 && (
                    <span className="px-2 py-0.5 bg-blue-50 text-blue-600 text-xs font-medium rounded-full animate-pulse">
                      {activeTransfers.length} 个传输中
                    </span>
                  )}
                </div>
                <div className="flex items-center gap-2 flex-wrap">
                  <select
                    value={selectedTask}
                    onChange={(e) => {
                      setSelectedTask(e.target.value);
                      setOutputLogsPage(1);
                    }}
                    className="px-3 py-2 bg-white border border-gray-300 text-gray-700 rounded-lg focus:ring-2 focus:ring-blue-500 text-sm"
                  >
                    <option value="">全部任务</option>
                    {tasks.map(task => (
                      <option key={task.id} value={task.id}>{task.name}</option>
                    ))}
                  </select>
                  <button
                    onClick={handleRefresh}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-white border border-gray-300 text-gray-600 rounded-lg hover:bg-gray-50 transition-colors text-sm"
                  >
                    <RotateCcw className="w-3.5 h-3.5" />
                    刷新
                  </button>
                  <button
                    onClick={handleCleanOutputLogs}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-red-50 text-red-600 border border-red-200 rounded-lg hover:bg-red-100 transition-colors text-sm"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                    清空
                  </button>
                </div>
              </>
            ) : (
              <>
                <div className="flex items-center gap-2 md:gap-3">
                  <FileText className="w-5 h-5 text-blue-500" />
                  <h2 className="text-base md:text-lg font-semibold text-gray-900">任务日志</h2>
                </div>
                <div className="flex items-center gap-2 flex-wrap">
                  <select
                    value={selectedTask}
                    onChange={(e) => {
                      setSelectedTask(e.target.value);
                      loadTaskLog(e.target.value);
                    }}
                    className="px-3 py-2 bg-white border border-gray-300 text-gray-700 rounded-lg focus:ring-2 focus:ring-blue-500 text-sm"
                  >
                    <option value="">选择任务</option>
                    {tasks.map(task => (
                      <option key={task.id} value={task.id}>{task.name}</option>
                    ))}
                  </select>
                  <button
                    onClick={handleRefresh}
                    className="inline-flex items-center gap-2 px-3 py-2 bg-white border border-gray-300 text-gray-600 rounded-lg hover:bg-gray-50 transition-colors text-sm"
                  >
                    <RotateCcw className="w-3.5 h-3.5" />
                    刷新
                  </button>
                </div>
              </>
            )}
          </div>

          {/* Content */}
          {activeTab === 'records' ? (
            <div>
              {/* Active transfers (mobile-friendly cards) */}
              {activeTransfers.length > 0 && (
                <div className="mb-4">
                  <h3 className="text-sm font-medium text-gray-500 mb-2 flex items-center gap-2">
                    <span className="w-2 h-2 bg-blue-500 rounded-full animate-pulse" />
                    正在传输 ({activeTransfers.length})
                  </h3>
                  <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {activeTransfers.map((t, idx) => (
                      <div key={`${t.task_id}_${t.file_name}_${idx}`} className="bg-blue-50 rounded-lg p-3 border border-blue-100">
                        <div className="flex items-center gap-2 mb-2">
                          <HardDrive className="w-4 h-4 text-blue-500 flex-shrink-0" />
                          <span className="text-sm font-medium text-gray-800 truncate" title={t.file_name}>{t.file_name}</span>
                        </div>
                        <ProgressBar progress={t.progress || 0} bytes={t.bytes || 0} size={t.size || 0} speed={t.speed || 0} />
                        <div className="text-xs text-gray-400 mt-1.5">
                          任务: {tasks.find(task => task.id === t.task_id)?.name || `任务#${t.task_id}`}
                        </div>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* Desktop Table / Mobile Cards */}
              <div className="hidden md:block overflow-x-auto">
                <table className="w-full text-sm text-left">
                  <thead className="text-xs text-gray-500 uppercase bg-gray-50 border-b border-gray-200">
                    <tr>
                      <th className="px-4 py-3 rounded-tl-lg w-16">状态</th>
                      <th className="px-4 py-3">文件</th>
                      <th className="px-4 py-3">源路径</th>
                      <th className="px-4 py-3">目标路径</th>
                      <th className="px-4 py-3">大小</th>
                      <th className="px-4 py-3">进度</th>
                      <th className="px-4 py-3 w-28">时间</th>
                      <th className="px-4 py-3 rounded-tr-lg w-16">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {loading ? (
                      <tr>
                        <td colSpan="8" className="px-4 py-8 text-center text-gray-400">
                          <div className="flex items-center justify-center">
                            <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-blue-500"></div>
                          </div>
                        </td>
                      </tr>
                    ) : outputLogs.length === 0 && activeTransfers.length === 0 ? (
                      <tr>
                        <td colSpan="8" className="px-4 py-8 text-center text-gray-400">
                          <List className="w-8 h-8 mx-auto mb-2 opacity-50" />
                          <p>暂无转移记录</p>
                          <p className="text-xs mt-1">执行任务后，文件传输记录将显示在这里</p>
                        </td>
                      </tr>
                    ) : (
                      outputLogs.map((log, idx) => (
                        <tr
                          key={log.id}
                          className={`border-b border-gray-100 hover:bg-gray-50 transition-colors ${
                            idx === outputLogs.length - 1 ? 'border-b-0' : ''
                          }`}
                        >
                          <td className="px-4 py-3">
                            {log.status ? (
                              <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-green-100 text-green-700">
                                成功
                              </span>
                            ) : (
                              <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-red-100 text-red-700">
                                失败
                              </span>
                            )}
                          </td>
                          <td className="px-4 py-3">
                            <div className="text-gray-800 font-medium max-w-[180px] truncate" title={log.file_name}>
                              {log.file_name || '-'}
                            </div>
                            {log.file_ext && (
                              <span className="text-xs text-gray-400">{log.file_ext}</span>
                            )}
                          </td>
                          <td className="px-4 py-3">
                            <div className="text-gray-600 max-w-[180px] truncate" title={log.src}>
                              {log.src || '-'}
                            </div>
                          </td>
                          <td className="px-4 py-3">
                            <div className="text-gray-600 max-w-[180px] truncate" title={log.dest}>
                              {log.dest || '-'}
                            </div>
                          </td>
                          <td className="px-4 py-3 text-gray-600 whitespace-nowrap">
                            {formatFileSize(log.file_size)}
                          </td>
                          <td className="px-4 py-3">
                            <ProgressBar progress={100} bytes={0} size={log.file_size || 0} speed={0} />
                          </td>
                          <td className="px-4 py-3 text-gray-400 whitespace-pre-line text-xs">
                            {formatDate(log.date)}
                          </td>
                          <td className="px-4 py-3">
                            <button
                              onClick={() => handleDeleteOutputLog(log.id)}
                              className="text-gray-400 hover:text-red-500 transition-colors"
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
              </div>

              {/* Mobile Cards */}
              <div className="md:hidden space-y-3">
                {loading ? (
                  <div className="py-8 text-center text-gray-400">
                    <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-blue-500 mx-auto"></div>
                  </div>
                ) : outputLogs.length === 0 && activeTransfers.length === 0 ? (
                  <div className="py-8 text-center text-gray-400">
                    <List className="w-8 h-8 mx-auto mb-2 opacity-50" />
                    <p>暂无转移记录</p>
                    <p className="text-xs mt-1">执行任务后，文件传输记录将显示在这里</p>
                  </div>
                ) : (
                  outputLogs.map((log) => (
                    <div key={log.id} className="bg-white rounded-lg border border-gray-200 p-4 space-y-3">
                      <div className="flex items-center justify-between">
                        <div className="flex items-center gap-2">
                          {log.status ? (
                            <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-green-100 text-green-700">
                              成功
                            </span>
                          ) : (
                            <span className="inline-flex items-center px-2 py-1 rounded text-xs font-medium bg-red-100 text-red-700">
                              失败
                            </span>
                          )}
                          <span className="text-sm font-medium text-gray-800 truncate max-w-[150px]" title={log.file_name}>
                            {log.file_name || '-'}
                          </span>
                        </div>
                        <button
                          onClick={() => handleDeleteOutputLog(log.id)}
                          className="text-gray-400 hover:text-red-500 transition-colors"
                          title="删除"
                        >
                          <Trash2 className="w-4 h-4" />
                        </button>
                      </div>
                      <ProgressBar progress={100} bytes={0} size={log.file_size || 0} speed={0} />
                      <div className="grid grid-cols-2 gap-2 text-xs text-gray-500">
                        <div>
                          <span className="text-gray-400">大小:</span> {formatFileSize(log.file_size)}
                        </div>
                        <div>
                          <span className="text-gray-400">时间:</span> {formatDate(log.date).replace('\n', ' ')}
                        </div>
                      </div>
                      <div className="text-xs text-gray-500 truncate" title={log.src}>
                        <span className="text-gray-400">源:</span> {log.src || '-'}
                      </div>
                      <div className="text-xs text-gray-500 truncate" title={log.dest}>
                        <span className="text-gray-400">目标:</span> {log.dest || '-'}
                      </div>
                    </div>
                  ))
                )}
              </div>

              {/* Pagination */}
              {!loading && outputLogsTotal > 0 && (
                <div className="flex flex-col sm:flex-row items-center justify-between gap-3 px-2 md:px-4 py-3 border-t border-gray-100 mt-2">
                  <div className="text-sm text-gray-500">
                    共 {outputLogsTotal} 条记录
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => setOutputLogsPage(p => Math.max(1, p - 1))}
                      disabled={outputLogsPage <= 1}
                      className="p-2 bg-white border border-gray-300 text-gray-600 rounded-lg hover:bg-gray-50 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                    >
                      <ChevronLeft className="w-4 h-4" />
                    </button>
                    <span className="text-sm text-gray-600 px-2">
                      第 {outputLogsPage} 页
                      {outputLogsTotal > 0 && `（${Math.min(outputLogsTotal, 20)} 条）`}
                    </span>
                    <button
                      onClick={() => setOutputLogsPage(p => p + 1)}
                      disabled={outputLogsPage * 20 >= outputLogsTotal}
                      className="p-2 bg-white border border-gray-300 text-gray-600 rounded-lg hover:bg-gray-50 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                    >
                      <ChevronRight className="w-4 h-4" />
                    </button>
                    <button
                      onClick={handleRefresh}
                      className="inline-flex items-center gap-1 px-3 py-2 bg-white border border-gray-300 text-gray-600 rounded-lg hover:bg-gray-50 transition-colors text-sm ml-2"
                    >
                      <RotateCcw className="w-3.5 h-3.5" />
                      刷新
                    </button>
                  </div>
                </div>
              )}
            </div>
          ) : (
            /* Raw log viewer (task tab) */
            <>
              {/* Search */}
              <div className="relative mb-4">
                <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
                <input
                  type="text"
                  placeholder="搜索日志内容..."
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  className="w-full pl-10 pr-4 py-2 bg-white border border-gray-300 text-gray-800 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 placeholder-gray-400"
                />
              </div>

              {/* Log content */}
              <div className="bg-gray-50 rounded-lg h-[400px] md:h-[600px] overflow-auto font-mono text-sm border border-gray-200">
                {loading ? (
                  <div className="flex items-center justify-center h-full text-gray-400">
                    <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-500"></div>
                  </div>
                ) : filteredLogs.length === 0 ? (
                  <div className="flex items-center justify-center h-full text-gray-400">
                    <div className="text-center">
                      <FileText className="w-8 h-8 mx-auto mb-2 opacity-50" />
                      <p>暂无日志</p>
                    </div>
                  </div>
                ) : (
                  <div className="divide-y divide-gray-200">
                    {filteredLogs.map((line, idx) => (
                      <div
                        key={idx}
                        className={`px-3 md:px-4 py-2 text-xs md:text-sm ${
                          line.includes('ERROR') ? 'text-red-600 bg-red-50' :
                          line.includes('WARN') ? 'text-amber-600 bg-amber-50' :
                          line.includes('Transferred') || line.includes('success') ? 'text-green-600 bg-green-50' :
                          'text-gray-700'
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
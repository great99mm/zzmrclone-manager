import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Folder,
  File,
  ChevronRight,
  Home,
  ArrowLeft,
  Move,
  Copy,
  X,
  Loader2,
  AlertTriangle,
} from 'lucide-react';
import {
  listLocalFiles,
  listRemoteFiles,
  getRemoteDetails,
  createQuickTask,
  getQuickTasks,
  deleteTask,
  createRemoteDir,
  createWebSocket,
} from '../services/api';
import toast from 'react-hot-toast';

const LOCAL_ROOT_PATH = '/';
const LOCAL_DEFAULT_PATH = '/';

const normalizeRemotePath = (path) => {
  if (!path) return '/';
  const normalized = path.startsWith('/') ? path : `/${path}`;
  return normalized.replace(/\/+/g, '/');
};

const joinRemotePath = (base, name) => {
  const cleanBase = normalizeRemotePath(base).replace(/\/$/, '');
  return `${cleanBase || ''}/${name}`.replace(/\/+/g, '/');
};

const buildPathCrumbs = (path, rootName) => {
  const cleanPath = normalizeRemotePath(path);
  const crumbs = [{ name: rootName, path: '/' }];
  if (cleanPath === '/') return crumbs;
  let acc = '';
  cleanPath.split('/').filter(Boolean).forEach((part) => {
    acc += `/${part}`;
    crumbs.push({ name: part, path: acc });
  });
  return crumbs;
};

const FileBrowser = () => {
  const navigate = useNavigate();
  const [roots, setRoots] = useState([{ key: 'local', type: 'local', name: '本地目录', path: LOCAL_ROOT_PATH, startPath: LOCAL_DEFAULT_PATH }]);
  const [currentRoot, setCurrentRoot] = useState(null);
  const [currentPath, setCurrentPath] = useState('/');
  const [items, setItems] = useState([]);
  const [loading, setLoading] = useState(false);
  const [selectedItems, setSelectedItems] = useState(new Set());
  const [showActionModal, setShowActionModal] = useState(false);
  const [showMoveConfirm, setShowMoveConfirm] = useState(false);
  const [actionMode, setActionMode] = useState('copy');
  const [destPath, setDestPath] = useState('');
  const [destType, setDestType] = useState('local');
  const [destRemote, setDestRemote] = useState('');
  const [destRemotePath, setDestRemotePath] = useState('/');
  const [destRemoteItems, setDestRemoteItems] = useState([]);
  const [destRemoteLoading, setDestRemoteLoading] = useState(false);
  const [destNewFolderName, setDestNewFolderName] = useState('');
  const [destCreatingDir, setDestCreatingDir] = useState(false);

  const [quickTasks, setQuickTasks] = useState([]);
  const [quickTaskProgress, setQuickTaskProgress] = useState({});
  const [deletingQuickTaskId, setDeletingQuickTaskId] = useState(null);
  const [processing, setProcessing] = useState(false);

  useEffect(() => {
    getRemoteDetails()
      .then((res) => {
        const remotes = ((res.data || {}).remotes || []).map((r) => ({
          key: r.name,
          type: 'remote',
          name: r.name,
          path: '/',
        }));
        setRoots([{ key: 'local', type: 'local', name: '本地目录', path: LOCAL_ROOT_PATH, startPath: LOCAL_DEFAULT_PATH }, ...remotes]);
      })
      .catch(() => {
        setRoots([{ key: 'local', type: 'local', name: '本地目录', path: LOCAL_ROOT_PATH, startPath: LOCAL_DEFAULT_PATH }]);
      });
  }, []);

  const loadQuickTasks = useCallback(async () => {
    try {
      const res = await getQuickTasks();
      setQuickTasks(res.data || []);
    } catch {
      setQuickTasks([]);
    }
  }, []);

  useEffect(() => {
    loadQuickTasks();
    const interval = setInterval(loadQuickTasks, 3000);
    return () => clearInterval(interval);
  }, [loadQuickTasks]);

  useEffect(() => {
    const ws = createWebSocket();

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
            [data.task_id]: {
              ...(prev[data.task_id] || {}),
              progress: 100,
              speed: 0,
            },
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
      } catch {
        // ignore malformed websocket messages
      }
    };

    return () => ws.close();
  }, [loadQuickTasks]);

  const currentRootMeta = useMemo(
    () => roots.find((r) => r.key === currentRoot) || null,
    [roots, currentRoot]
  );

  const remoteRoots = useMemo(
    () => roots.filter((r) => r.type === 'remote'),
    [roots]
  );

  const destRemoteMeta = useMemo(
    () => remoteRoots.find((r) => r.name === destRemote) || null,
    [remoteRoots, destRemote]
  );

  const runningQuickTasks = useMemo(
    () => quickTasks.filter((task) => task.status === 'running'),
    [quickTasks]
  );

  const finishedQuickTasks = useMemo(
    () => quickTasks.filter((task) => task.status !== 'running' && (task.last_run || task.status === 'error')),
    [quickTasks]
  );

  const isRootHome = currentRoot === null;

  const loadItems = useCallback(async () => {
    if (isRootHome || !currentRootMeta) {
      setItems([]);
      setLoading(false);
      return;
    }

    setLoading(true);
    try {
      let res;
      if (currentRootMeta.type === 'local') {
        res = await listLocalFiles(currentPath);
      } else {
        res = await listRemoteFiles(currentRootMeta.name, currentPath);
      }
      const list = (res.data.items || []).slice();
      list.sort((a, b) => {
        if (a.is_dir && !b.is_dir) return -1;
        if (!a.is_dir && b.is_dir) return 1;
        return a.name.localeCompare(b.name);
      });
      setItems(list);
    } catch (err) {
      toast.error('加载目录失败');
      setItems([]);
    } finally {
      setLoading(false);
    }
  }, [isRootHome, currentRootMeta, currentPath]);

  useEffect(() => {
    loadItems();
    setSelectedItems(new Set());
  }, [loadItems]);

  useEffect(() => {
    if (destType === 'remote' && !destRemote && remoteRoots.length > 0) {
      setDestRemote(remoteRoots[0].name);
      setDestRemotePath('/');
    }
  }, [destType, destRemote, remoteRoots]);

  const loadDestRemoteDir = useCallback(async () => {
    if (!showActionModal || destType !== 'remote' || !destRemote) {
      setDestRemoteItems([]);
      return;
    }
    setDestRemoteLoading(true);
    try {
      const res = await listRemoteFiles(destRemote, destRemotePath || '/');
      const dirs = (res.data.items || [])
        .filter((item) => item.is_dir)
        .sort((a, b) => a.name.localeCompare(b.name));
      setDestRemoteItems(dirs);
    } catch (err) {
      toast.error('加载目录失败');
      setDestRemoteItems([]);
    } finally {
      setDestRemoteLoading(false);
    }
  }, [showActionModal, destType, destRemote, destRemotePath]);

  useEffect(() => {
    loadDestRemoteDir();
  }, [loadDestRemoteDir]);

  const openRoot = (root) => {
    setCurrentRoot(root.key);
    setCurrentPath(root.startPath || root.path || '/');
    setSelectedItems(new Set());
  };

  const navigateTo = (item) => {
    if (!item.is_dir) return;
    setCurrentPath(item.path);
  };

  const goUp = () => {
    const rootPath = currentRootMeta?.path || '/';
    if (currentPath === '/' || (currentRootMeta?.type !== 'local' && currentPath === rootPath)) return;
    const parts = currentPath.split('/').filter(Boolean);
    parts.pop();
    const nextPath = parts.length ? `/${parts.join('/')}` : '/';
    if (currentRootMeta?.type === 'local' && !nextPath.startsWith(rootPath)) {
      setCurrentPath(rootPath);
      return;
    }
    setCurrentPath(nextPath);
  };

  const toggleSelect = (item) => {
    const key = `${item.path}|${item.is_dir}`;
    const next = new Set(selectedItems);
    if (next.has(key)) {
      next.delete(key);
    } else {
      next.add(key);
    }
    setSelectedItems(next);
  };

  const breadcrumbs = () => {
    if (!currentRootMeta) return [];
    const rootPath = currentRootMeta.path || '/';
    if (currentPath === rootPath) return [{ name: currentRootMeta.name, path: rootPath }];
    const relativePath = rootPath !== '/' && currentPath.startsWith(`${rootPath}/`)
      ? currentPath.slice(rootPath.length)
      : currentPath;
    const parts = relativePath.split('/').filter(Boolean);
    const crumbs = [{ name: currentRootMeta.name, path: rootPath }];
    let acc = '';
    parts.forEach((part) => {
      acc += `/${part}`;
      crumbs.push({ name: part, path: rootPath === '/' ? acc : `${rootPath}${acc}` });
    });
    return crumbs;
  };

  const destBreadcrumbs = () => buildPathCrumbs(destRemotePath, destRemote || '云盘');

  const destGoUp = () => {
    const cleanPath = normalizeRemotePath(destRemotePath);
    if (cleanPath === '/') return;
    const parts = cleanPath.split('/').filter(Boolean);
    parts.pop();
    setDestRemotePath(parts.length ? `/${parts.join('/')}` : '/');
  };

  const handleDestRemoteChange = (remote) => {
    setDestRemote(remote);
    setDestRemotePath('/');
    setDestNewFolderName('');
  };

  const handleCreateDestDir = async () => {
    const name = destNewFolderName.trim();
    if (!destRemote) {
      toast.error('请选择云盘');
      return;
    }
    if (!name) {
      toast.error('请输入目录名');
      return;
    }
    if (name.includes('/')) {
      toast.error('目录名不能包含 /');
      return;
    }

    const newPath = joinRemotePath(destRemotePath, name);
    setDestCreatingDir(true);
    try {
      await createRemoteDir(destRemote, newPath);
      toast.success('目录已创建');
      setDestNewFolderName('');
      setDestRemotePath(newPath);
    } catch (err) {
      toast.error(err.response?.data?.error || '创建目录失败');
    } finally {
      setDestCreatingDir(false);
    }
  };

  const handleAction = async (confirmedMove = false) => {
    if (selectedItems.size === 0) {
      toast.error('请先选择文件或文件夹');
      return;
    }
    const finalDestPath = destType === 'remote'
      ? (destRemote ? `${destRemote}:${normalizeRemotePath(destRemotePath)}` : '')
      : destPath.trim();

    if (!finalDestPath) {
      toast.error(destType === 'remote' ? '请选择目标云盘' : '请输入目标路径');
      return;
    }
    if (!currentRootMeta) return;

    if (actionMode === 'move' && !confirmedMove) {
      setShowMoveConfirm(true);
      return;
    }

    setProcessing(true);
    const selected = items.filter((i) => selectedItems.has(`${i.path}|${i.is_dir}`));

    try {
      const createdTasks = [];
      for (const item of selected) {
        const source = currentRootMeta.type === 'local'
          ? item.path
          : `${currentRootMeta.name}:${item.path}`;

        const res = await createQuickTask({
          name: `${actionMode === 'copy' ? '复制' : '移动'} ${item.name}`,
          source,
          source_type: currentRootMeta.type === 'local' ? 'local' : 'remote',
          dest: finalDestPath,
          dest_type: destType,
          transfer_mode: actionMode,
          openlist_enabled: false,
          openlist_config_id: 0,
        });
        if (res?.data) {
          createdTasks.push({ ...res.data, status: 'running', last_error: '' });
        }
      }

      if (createdTasks.length > 0) {
        setQuickTasks((prev) => {
          const merged = [...createdTasks, ...prev.filter((task) => !createdTasks.some((created) => created.id === task.id))];
          return merged;
        });
      }

      toast.success(`已创建 ${selected.length} 个${actionMode === 'copy' ? '复制' : '移动'}任务`);
      loadQuickTasks();
      setShowActionModal(false);
      setShowMoveConfirm(false);
      setSelectedItems(new Set());
      setDestPath('');
      setDestRemote('');
      setDestRemotePath('/');
      setDestNewFolderName('');
      setOpenlistEnabled(false);
      setOpenlistConfigId('');
      setOpenlistRefreshDir('');
      setOpenlistMapping('');
    } catch (err) {
      toast.error(err.response?.data?.error || '创建任务失败');
    } finally {
      setProcessing(false);
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

  const formatSize = (bytes) => {
    if (!bytes) return '-';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let index = 0;
    let size = bytes;
    while (size >= 1024 && index < units.length - 1) {
      size /= 1024;
      index += 1;
    }
    return `${size.toFixed(1)} ${units[index]}`;
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">文件浏览器</h1>
        </div>
      </div>

      <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden min-h-[420px]">
          {isRootHome ? (
            <div className="p-6 md:p-8">
              <div className="mb-5">
                <h2 className="text-lg font-semibold text-gray-900">存储列表</h2>
              </div>

              <div className="flex flex-wrap gap-3">
                {roots.map((root) => (
                  <button
                    key={root.key}
                    onClick={() => openRoot(root)}
                    className="group inline-flex items-center gap-2.5 rounded-xl border border-gray-200 bg-white hover:bg-gray-50 hover:border-blue-300 hover:shadow-sm transition-all px-4 py-2.5"
                  >
                    <div className="w-8 h-8 rounded-lg bg-blue-50 flex items-center justify-center shrink-0">
                      <Folder className="w-4 h-4 text-blue-600" />
                    </div>
                    <div className="font-semibold text-gray-900 text-sm">{root.name}</div>
                  </button>
                ))}
              </div>
            </div>
          ) : (
            <>
              <div className="flex flex-col gap-3 px-4 py-3 border-b bg-gray-50">
                <div className="flex items-center justify-between gap-3">
                  <div className="flex items-center gap-1 text-sm text-gray-600 min-w-0 flex-wrap">
                    <button
                      onClick={() => {
                        setCurrentRoot(null);
                        setCurrentPath('/');
                      }}
                      className="p-1 hover:bg-gray-200 rounded"
                    >
                      <Home className="w-4 h-4" />
                    </button>
                    {currentRootMeta && currentPath !== (currentRootMeta.path || '/') && (
                      <button onClick={goUp} className="p-1 hover:bg-gray-200 rounded">
                        <ArrowLeft className="w-4 h-4" />
                      </button>
                    )}
                    {breadcrumbs().map((crumb, i) => (
                      <React.Fragment key={i}>
                        {i > 0 && <ChevronRight className="w-3 h-3 text-gray-400" />}
                        <button
                          onClick={() => setCurrentPath(crumb.path)}
                          className="hover:text-blue-600 px-1 py-0.5 rounded hover:bg-gray-200"
                        >
                          {crumb.name}
                        </button>
                      </React.Fragment>
                    ))}
                  </div>

                  {selectedItems.size > 0 && (
                    <div className="flex gap-2 shrink-0">
                      <button
                        onClick={() => {
                          setActionMode('copy');
                          setShowActionModal(true);
                        }}
                        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-blue-600 text-white rounded-lg text-sm font-medium hover:bg-blue-700"
                      >
                        <Copy className="w-3.5 h-3.5" /> 复制到
                      </button>
                      <button
                        onClick={() => {
                          setActionMode('move');
                          setShowActionModal(true);
                        }}
                        className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-gray-700 text-white rounded-lg text-sm font-medium hover:bg-gray-800"
                      >
                        <Move className="w-3.5 h-3.5" /> 移动到
                      </button>
                    </div>
                  )}
                </div>
              </div>

              <div className="divide-y">
                {loading ? (
                  <div className="flex items-center justify-center py-12">
                    <Loader2 className="w-6 h-6 animate-spin text-blue-600" />
                  </div>
                ) : items.length === 0 ? (
                  <div className="text-center py-12 text-gray-400 text-sm">此目录为空</div>
                ) : (
                  items.map((item, idx) => (
                    <div
                      key={idx}
                      className={`flex items-center gap-3 px-4 py-2.5 hover:bg-gray-50 cursor-pointer transition-colors ${
                        selectedItems.has(`${item.path}|${item.is_dir}`) ? 'bg-blue-50' : ''
                      }`}
                      onClick={() => (item.is_dir ? navigateTo(item) : toggleSelect(item))}
                    >
                      <input
                        type="checkbox"
                        checked={selectedItems.has(`${item.path}|${item.is_dir}`)}
                        onClick={(e) => e.stopPropagation()}
                        onChange={(e) => {
                          e.stopPropagation();
                          toggleSelect(item);
                        }}
                        className="w-4 h-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                      />
                      <div className="shrink-0">
                        {item.is_dir ? (
                          <Folder className="w-5 h-5 text-yellow-500" />
                        ) : (
                          <File className="w-5 h-5 text-gray-400" />
                        )}
                      </div>
                      <div className="flex-1 min-w-0">
                        <div className={`text-sm truncate ${item.is_dir ? 'text-blue-700 font-medium' : 'text-gray-800'}`}>
                          {item.name}
                        </div>
                      </div>
                      <div className="text-xs text-gray-400 w-24 text-right shrink-0">
                        {item.is_dir ? '文件夹' : formatSize(item.size)}
                      </div>
                    </div>
                  ))
                )}
              </div>
            </>
          )}
      </div>

      {(runningQuickTasks.length > 0 || finishedQuickTasks.length > 0) && (
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
          <div className="px-4 py-3 border-b bg-gray-50">
            <h2 className="text-lg font-semibold text-gray-900">文件浏览器任务</h2>
          </div>

          {runningQuickTasks.length > 0 && (
            <div className="p-4 space-y-3">
              {runningQuickTasks.map((task) => {
                const progressInfo = quickTaskProgress[task.id] || {};
                const progress = Math.max(0, Math.min(100, Number(progressInfo.progress || 0)));
                const destLabel = task.dest_type === 'local'
                  ? task.remote_dir
                  : `${task.remote_name}:${task.remote_dir}`;

                return (
                  <div
                    key={task.id}
                    onClick={() => navigate(`/tasks/${task.id}`)}
                    className="rounded-xl border border-blue-100 bg-blue-50/50 p-4 cursor-pointer hover:border-blue-200 hover:bg-blue-50 transition-colors"
                  >
                    <div className="flex items-start justify-between gap-3 mb-2">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-gray-900 truncate">{task.name}</div>
                        <div className="text-xs text-gray-500 mt-1 break-all">{task.source_dir} → {destLabel}</div>
                      </div>
                      <div className="text-sm font-semibold text-blue-700 shrink-0">{progress.toFixed(1)}%</div>
                    </div>

                    <div className="h-2 rounded-full bg-blue-100 overflow-hidden">
                      <div className="h-full bg-blue-600 rounded-full transition-all duration-300" style={{ width: `${progress}%` }} />
                    </div>

                    <div className="mt-2 text-xs text-gray-500 truncate">{progressInfo.fileName || '传输中'}</div>
                  </div>
                );
              })}
            </div>
          )}

          {finishedQuickTasks.length > 0 && (
            <div className={`${runningQuickTasks.length > 0 ? 'border-t' : ''} p-4 space-y-2`}>
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
                    className={`flex items-center gap-3 rounded-xl border px-3 py-2.5 ${cardClass}`}
                  >
                    <div
                      className="flex-1 min-w-0 cursor-pointer"
                      onClick={() => navigate(`/tasks/${task.id}`)}
                    >
                      <div className={`text-sm font-medium truncate ${textClass}`}>{task.name}</div>
                      <div className={`text-xs mt-0.5 truncate ${subClass}`}>
                        {statusText}
                      </div>
                    </div>
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

      {showActionModal && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div className="absolute inset-0 bg-black/50" onClick={() => {
            setShowMoveConfirm(false);
            setShowActionModal(false);
          }} />
          <div className="relative bg-white rounded-xl shadow-xl border border-gray-200 w-full max-w-2xl mx-4 p-6 max-h-[90vh] overflow-y-auto">
            <div className="flex items-center justify-between mb-5">
              <h3 className="text-lg font-semibold text-gray-900">
                {actionMode === 'copy' ? '复制' : '移动'} {selectedItems.size} 个项目
              </h3>
              <button onClick={() => {
                setShowMoveConfirm(false);
                setShowActionModal(false);
              }} className="p-1.5 text-gray-400 hover:text-gray-600">
                <X className="w-5 h-5" />
              </button>
            </div>

            <div className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">目标类型</label>
                <div className="flex gap-2">
                  <button
                    type="button"
                    onClick={() => setDestType('local')}
                    className={`flex-1 py-2 rounded-lg border text-sm font-medium ${
                      destType === 'local' ? 'border-blue-500 bg-blue-50 text-blue-700' : 'border-gray-200'
                    }`}
                  >
                    本地
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      setDestType('remote');
                      if (!destRemote && remoteRoots.length > 0) {
                        handleDestRemoteChange(remoteRoots[0].name);
                      }
                    }}
                    className={`flex-1 py-2 rounded-lg border text-sm font-medium ${
                      destType === 'remote' ? 'border-blue-500 bg-blue-50 text-blue-700' : 'border-gray-200'
                    }`}
                  >
                    云盘
                  </button>
                </div>
              </div>

              {destType === 'local' ? (
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">目标路径</label>
                  <input
                    type="text"
                    value={destPath}
                    onChange={(e) => setDestPath(e.target.value)}
                    placeholder="/backup/media"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                  />
                </div>
              ) : (
                <div className="space-y-3">
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">目标云盘</label>
                    <select
                      value={destRemote}
                      onChange={(e) => handleDestRemoteChange(e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    >
                      <option value="">选择云盘</option>
                      {remoteRoots.map((root) => (
                        <option key={root.name} value={root.name}>{root.name}</option>
                      ))}
                    </select>
                  </div>

                  {destRemote && (
                    <div className="border border-gray-200 rounded-xl overflow-hidden">
                      <div className="flex items-center justify-between gap-2 px-3 py-2 bg-gray-50 border-b">
                        <div className="flex items-center gap-1 text-sm text-gray-600 min-w-0 flex-wrap">
                          {destRemotePath !== '/' && (
                            <button type="button" onClick={destGoUp} className="p-1 hover:bg-gray-200 rounded">
                              <ArrowLeft className="w-4 h-4" />
                            </button>
                          )}
                          {destBreadcrumbs().map((crumb, i) => (
                            <React.Fragment key={`${crumb.path}-${i}`}>
                              {i > 0 && <ChevronRight className="w-3 h-3 text-gray-400" />}
                              <button
                                type="button"
                                onClick={() => setDestRemotePath(crumb.path)}
                                className="hover:text-blue-600 px-1 py-0.5 rounded hover:bg-gray-200"
                              >
                                {i === 0 ? (destRemoteMeta ? destRemoteMeta.name : crumb.name) : crumb.name}
                              </button>
                            </React.Fragment>
                          ))}
                        </div>
                        <div className="text-xs text-gray-500 shrink-0">{destRemote}:{normalizeRemotePath(destRemotePath)}</div>
                      </div>

                      <div className="max-h-52 overflow-y-auto divide-y">
                        {destRemoteLoading ? (
                          <div className="flex items-center justify-center py-8">
                            <Loader2 className="w-5 h-5 animate-spin text-blue-600" />
                          </div>
                        ) : destRemoteItems.length === 0 ? (
                          <div className="text-center py-8 text-gray-400 text-sm">此目录为空</div>
                        ) : (
                          destRemoteItems.map((item) => (
                            <button
                              key={item.path}
                              type="button"
                              onClick={() => setDestRemotePath(item.path)}
                              className="w-full flex items-center gap-2 px-3 py-2.5 text-left hover:bg-gray-50"
                            >
                              <Folder className="w-4 h-4 text-yellow-500" />
                              <span className="text-sm text-gray-800 truncate">{item.name}</span>
                            </button>
                          ))
                        )}
                      </div>

                      <div className="flex gap-2 p-3 bg-gray-50 border-t">
                        <input
                          type="text"
                          value={destNewFolderName}
                          onChange={(e) => setDestNewFolderName(e.target.value)}
                          placeholder="新目录名"
                          className="flex-1 min-w-0 px-3 py-2 border border-gray-300 rounded-lg text-sm"
                        />
                        <button
                          type="button"
                          onClick={handleCreateDestDir}
                          disabled={destCreatingDir}
                          className="px-3 py-2 bg-gray-700 text-white rounded-lg text-sm font-medium hover:bg-gray-800 disabled:opacity-50 flex items-center gap-1.5"
                        >
                          {destCreatingDir && <Loader2 className="w-3.5 h-3.5 animate-spin" />}
                          创建
                        </button>
                      </div>
                    </div>
                  )}
                </div>
              )}

              <button
                onClick={() => handleAction()}
                disabled={processing}
                className="w-full py-2.5 bg-blue-600 text-white rounded-lg font-medium hover:bg-blue-700 disabled:opacity-50 flex items-center justify-center gap-2"
              >
                {processing ? (
                  <Loader2 className="w-4 h-4 animate-spin" />
                ) : (
                  <>
                    {actionMode === 'copy' ? <Copy className="w-4 h-4" /> : <Move className="w-4 h-4" />}
                    确认{actionMode === 'copy' ? '复制' : '移动'}
                  </>
                )}
              </button>
            </div>
          </div>
        </div>
      )}

      {showMoveConfirm && (
        <div className="fixed inset-0 z-[60] flex items-center justify-center">
          <div className="absolute inset-0 bg-black/50" onClick={() => setShowMoveConfirm(false)} />
          <div className="relative bg-white rounded-2xl shadow-2xl border border-red-100 w-full max-w-md mx-4 overflow-hidden">
            <div className="p-6">
              <div className="flex items-start gap-4">
                <div className="w-12 h-12 rounded-2xl bg-red-50 flex items-center justify-center shrink-0">
                  <AlertTriangle className="w-6 h-6 text-red-600" />
                </div>
                <div className="min-w-0">
                  <h3 className="text-lg font-semibold text-gray-900">确认移动</h3>
                  <p className="text-sm text-red-600 font-medium mt-2">移动后本地文件会丢失!请谨慎操作</p>
                </div>
              </div>
            </div>
            <div className="flex items-center justify-end gap-2 px-6 py-4 bg-gray-50 border-t border-gray-100">
              <button
                type="button"
                onClick={() => setShowMoveConfirm(false)}
                className="px-4 py-2 text-gray-700 hover:bg-gray-100 rounded-lg transition-colors font-medium text-sm"
              >
                取消
              </button>
              <button
                type="button"
                onClick={() => {
                  setShowMoveConfirm(false);
                  handleAction(true);
                }}
                disabled={processing}
                className="px-4 py-2 bg-red-600 text-white rounded-lg hover:bg-red-700 transition-colors font-medium text-sm disabled:opacity-50"
              >
                确认移动
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default FileBrowser;

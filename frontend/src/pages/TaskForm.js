import React, { useState, useEffect, useCallback } from 'react';
import { Link, useParams, useNavigate } from 'react-router-dom';
import { 
  ArrowLeft, 
  Save, 
  FolderOpen, 
  Cloud, 
  Settings2,
  Clock,
  X,
  Folder,
  ChevronRight,
  Home,
  HardDrive,
} from 'lucide-react';
import { createTask, updateTask, getTask, getManualAvailableAccounts, getRemotes, listRemoteDir } from '../services/api';
import toast from 'react-hot-toast';

const TaskForm = () => {
  const { id } = useParams();
  const navigate = useNavigate();
  const isEdit = !!id;

  const [form, setForm] = useState({
    name: '',
    task_type: 'normal',
    source_type: 'local',
    source_dir: '',
    dest_type: 'remote',
    remote_name: '',
    remote_dir: '',
    transfer_mode: 'move',
    transfers: 16,
    checkers: 32,
    bind_ip: '',
    rclone_config: '',
    enabled: true,
    min_age: '10s',
    drive_chunk_size: '256M',
    buffer_size: '512M',
    retries: 3,
    schedule_enabled: false,
    schedule_interval: 15,
    watch_enabled: true,
    qb_enabled: false,
    qb_url: '',
    qb_username: '',
    qb_password: '',
    qb_poll_interval: 60,
    qb_delete_files: false,
  });

  const [remotes, setRemotes] = useState([]);
  const [manualAccounts, setManualAccounts] = useState([]);
  const [manualAccountIds, setManualAccountIds] = useState([]);
  const [manualAccountsLoading, setManualAccountsLoading] = useState(false);
  const [manualAccountsError, setManualAccountsError] = useState('');

  const [loading, setLoading] = useState(isEdit);
  const [saving, setSaving] = useState(false);



  // Directory browser state
  const [browserOpen, setBrowserOpen] = useState(false);
  const [browserTarget, setBrowserTarget] = useState('source'); // 'source' or 'dest'
  const [browserRemote, setBrowserRemote] = useState('');
  const [browserPath, setBrowserPath] = useState('/');
  const [browserItems, setBrowserItems] = useState([]);
  const [browserLoading, setBrowserLoading] = useState(false);
  const [browserBreadcrumbs, setBrowserBreadcrumbs] = useState([{ name: '/', path: '/' }]);

  const isManualTask = form.task_type === 'manual';
  const isLegacyRotationTask = form.task_type === 'rotation';

  const loadRemotes = useCallback(async () => {
    try {
      const res = await getRemotes();
      setRemotes(res.data.remotes || []);
    } catch (err) {
      console.error('Failed to load remotes');
    }
  }, []);

  const loadTask = useCallback(async () => {
    try {
      const res = await getTask(id);
      setForm({
        ...res.data,
        task_type: res.data.task_type || 'normal',
        source_type: res.data.task_type === 'manual' ? 'local' : res.data.source_type,
        dest_type: res.data.task_type === 'manual' ? 'remote' : res.data.dest_type,
        transfer_mode: res.data.task_type === 'manual' && res.data.transfer_mode === 'sync' ? 'copy' : res.data.transfer_mode,
      });
    } catch (err) {
      toast.error('加载任务失败');
      navigate('/tasks');
    } finally {
      setLoading(false);
    }
  }, [id, navigate]);

  useEffect(() => {
    loadRemotes();
    if (isEdit) {
      loadTask();
    }
  }, [isEdit, loadRemotes, loadTask]);

  useEffect(() => {
    if (!isManualTask || isEdit) return undefined;
    let active = true;
    const loadManualAccounts = async () => {
      setManualAccountsLoading(true);
      setManualAccountsError('');
      try {
        const res = await getManualAvailableAccounts();
        if (active) setManualAccounts(res.data.accounts || []);
      } catch (err) {
        if (active) setManualAccountsError(err.response?.data?.error || '可信账号列表加载失败');
      } finally {
        if (active) setManualAccountsLoading(false);
      }
    };
    loadManualAccounts();
    return () => { active = false; };
  }, [isEdit, isManualTask]);

  // Directory browser functions
  const parseRemotePath = (input) => {
    if (!input) return { remote: '', path: '/' };
    const idx = input.indexOf(':');
    if (idx > 0) {
      return { remote: input.substring(0, idx), path: input.substring(idx + 1) || '/' };
    }
    return { remote: '', path: '/' };
  };

  const updateSourceRemote = (remote) => {
    const { path } = parseRemotePath(form.source_dir);
    handleChange('source_dir', remote ? `${remote}:${path || '/'}` : '');
  };

  const updateSourceRemotePath = (path) => {
    const { remote } = parseRemotePath(form.source_dir);
    handleChange('source_dir', remote ? `${remote}:${path || '/'}` : path);
  };

  const openBrowser = (target) => {
    let remote, currentPath;
    if (target === 'source') {
      if (form.source_type !== 'remote') return;
      const parsed = parseRemotePath(form.source_dir);
      remote = parsed.remote;
      currentPath = parsed.path;
    } else {
      if (form.dest_type !== 'remote') return;
      remote = form.remote_name;
      currentPath = form.remote_dir || '/';
    }
    if (!remote) {
      toast.error('请先输入或选择远程盘符');
      return;
    }
    setBrowserTarget(target);
    setBrowserRemote(remote);
    const initPath = currentPath || '/';
    setBrowserPath(initPath);
    setBrowserBreadcrumbs(buildBreadcrumbs(initPath));
    setBrowserOpen(true);
    loadBrowserDir(remote, initPath);
  };

  const buildBreadcrumbs = (path) => {
    if (!path || path === '/') return [{ name: '/', path: '/' }];
    const parts = path.split('/').filter(Boolean);
    const crumbs = [{ name: '/', path: '/' }];
    let accumulated = '';
    parts.forEach((part) => {
      accumulated += '/' + part;
      crumbs.push({ name: part, path: accumulated });
    });
    return crumbs;
  };

  const loadBrowserDir = async (remote, path) => {
    if (!remote) return;
    setBrowserLoading(true);
    try {
      const res = await listRemoteDir(remote, path || '/');
      const items = (res.data.items || []).filter(i => i.is_dir);
      setBrowserItems(items);
    } catch (err) {
      toast.error('加载目录失败');
      setBrowserItems([]);
    } finally {
      setBrowserLoading(false);
    }
  };

  const browserNavigate = (item) => {
    if (!item.is_dir) return;
    const newPath = item.path;
    setBrowserPath(newPath);
    setBrowserBreadcrumbs(buildBreadcrumbs(newPath));
    loadBrowserDir(browserRemote, newPath);
  };

  const browserGoToCrumb = (crumb) => {
    setBrowserPath(crumb.path);
    setBrowserBreadcrumbs(buildBreadcrumbs(crumb.path));
    loadBrowserDir(browserRemote, crumb.path);
  };

  const browserSelect = () => {
    if (browserTarget === 'source') {
      handleChange('source_dir', browserRemote + ':' + browserPath);
    } else {
      handleChange('remote_name', browserRemote);
      handleChange('remote_dir', browserPath);
    }
    setBrowserOpen(false);
  };

  const handleSubmit = async (e) => {
    e.preventDefault();



    if (form.qb_enabled && !isManualTask) {
      if (form.source_type !== 'local') {
        toast.error('qBittorrent 完成触发只支持本地源目录');
        return;
      }
      if (!form.qb_url.trim()) {
        toast.error('请填写 qBittorrent 地址');
        return;
      }
    }

    let submitForm = { ...form };

    if (isManualTask) {
      if (submitForm.source_type !== 'local') {
        toast.error('手动分配任务只支持本地源目录');
        return;
      }
      if (submitForm.dest_type !== 'remote') {
        toast.error('手动分配任务只支持云盘目标');
        return;
      }
      if (submitForm.transfer_mode !== 'copy' && submitForm.transfer_mode !== 'move') {
		toast.error('手动分配任务只支持复制或移动模式');
		return;
	  }
	  if (!isEdit && manualAccountIds.length === 0) {
		toast.error('请至少选择一个分配账号');
		return;
	  }
      submitForm = {
        ...submitForm,
        source_type: 'local',
        dest_type: 'remote',
        watch_enabled: false,
        schedule_enabled: false,
        qb_enabled: false,
        qb_url: '',
        qb_username: '',
        qb_password: '',
        qb_poll_interval: 60,
        qb_delete_files: false,
		...(isEdit ? {} : { manual_account_ids: manualAccountIds }),
      };
    }

    setSaving(true);

    try {
      if (isEdit) {
        await updateTask(id, submitForm);
        toast.success('任务已更新');
      } else {
        await createTask(submitForm);
        toast.success('任务已创建');
      }
      navigate('/tasks');
    } catch (err) {
      toast.error(err.response?.data?.error || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const handleChange = (field, value) => {
    setForm(prev => ({ ...prev, [field]: value }));
  };

  const handleTaskTypeChange = (taskType) => {
    setForm(prev => ({
      ...prev,
      task_type: taskType,
      source_type: taskType === 'manual' ? 'local' : prev.source_type,
      dest_type: taskType === 'manual' ? 'remote' : prev.dest_type,
      transfer_mode: taskType === 'manual' && prev.transfer_mode === 'sync' ? 'copy' : prev.transfer_mode,
      watch_enabled: taskType === 'manual' ? false : prev.watch_enabled,
      schedule_enabled: taskType === 'manual' ? false : prev.schedule_enabled,
      qb_enabled: taskType === 'manual' ? false : prev.qb_enabled,
    }));
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  if (isLegacyRotationTask) {
    return (
      <div className="max-w-2xl mx-auto">
        <div className="flex items-center gap-4 mb-6">
          <button type="button" onClick={() => navigate('/tasks')} className="p-2 hover:bg-gray-100 rounded-lg transition-colors" aria-label="返回任务列表">
            <ArrowLeft className="w-5 h-5" />
          </button>
          <div>
            <h1 className="text-2xl font-bold text-gray-900">任务配置不可用</h1>
            <p className="text-gray-500 mt-1">此任务使用已停止支持的任务类型，无法在此编辑。</p>
          </div>
        </div>
        <div className="bg-white rounded-xl shadow-sm border border-amber-200 p-6" role="status">
          <p className="text-sm text-gray-700">请返回任务列表。现有任务只能使用普通任务或手动分配任务。</p>
        </div>
      </div>
    );
  }

  return (
    <div className="max-w-4xl mx-auto">
      {/* Header */}
      <div className="flex items-center gap-4 mb-6">
        <button 
          onClick={() => navigate('/tasks')}
          className="p-2 hover:bg-gray-100 rounded-lg transition-colors"
        >
          <ArrowLeft className="w-5 h-5" />
        </button>
        <div>
          <h1 className="text-2xl font-bold text-gray-900">
            {isEdit ? '编辑任务' : '新建任务'}
          </h1>
          <p className="text-gray-500 mt-1">
            {isEdit ? '修改现有任务配置' : (isManualTask ? '配置新的手动分配任务' : '配置新的 Rclone 自动化任务')}
          </p>
        </div>
      </div>

      <form onSubmit={handleSubmit} className="space-y-6">
        {/* Basic Info */}
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
            <FolderOpen className="w-5 h-5 text-blue-500" />
            基本信息
          </h2>

          <div className="space-y-4">
            {/* Task name */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">
                任务名称 <span className="text-red-500">*</span>
              </label>
              <input
                type="text"
                required
                value={form.name}
                onChange={(e) => handleChange('name', e.target.value)}
                placeholder="例如：每日媒体同步"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-2">任务类型</label>
              <div className="grid grid-cols-1 md:grid-cols-2 gap-2">
                {[
                  { value: 'normal', label: '普通任务', desc: '普通任务保持现在逻辑' },
                  { value: 'manual', label: '手动分配任务', desc: '分析、分配并按账号独立运行' },
                ].map(type => (
                  <button
                    key={type.value}
                    type="button"
                    onClick={() => handleTaskTypeChange(type.value)}
                    className={`p-3 rounded-lg border-2 text-left transition-colors ${
                      form.task_type === type.value ? 'border-blue-500 bg-blue-50' : 'border-gray-200 hover:border-gray-300'
                    }`}
                  >
                    <div className="font-medium text-sm text-gray-900">{type.label}</div>
                    <div className="text-xs text-gray-500 mt-0.5">{type.desc}</div>
                  </button>
                ))}
              </div>
            </div>

            {isManualTask && (
              <div className="border-t pt-4 text-sm text-gray-600">
                手动分配任务不会自动执行；创建时选择账号，保存后可在任务详情中调整顺序、分析文件并生成预览。
              </div>
            )}

            {isManualTask && !isEdit && (
              <fieldset className="border-t pt-4" aria-describedby="manual-accounts-help">
                <legend className="text-sm font-medium text-gray-700">分配账号 <span className="text-red-500">*</span></legend>
                <p id="manual-accounts-help" className="mt-1 text-xs text-gray-500">选择本次手动分配会使用的可信账号。创建后可在任务详情调整顺序。</p>
                {manualAccountsLoading && <div className="py-3 text-sm text-gray-500">正在加载可信账号...</div>}
                {manualAccountsError && <div className="py-3 text-sm text-red-600" role="alert">{manualAccountsError}</div>}
                {!manualAccountsLoading && !manualAccountsError && manualAccounts.length === 0 && <div className="py-3 text-sm text-amber-700">没有可用的可信账号。<Link to="/accounts" className="ml-2 font-medium text-blue-700 hover:text-blue-800">前往账号管理</Link></div>}
                {!manualAccountsLoading && manualAccounts.length > 0 && (
                  <div className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2">
                    {manualAccounts.map(account => {
                      const checked = manualAccountIds.includes(account.account_id);
                      return (
                        <label key={account.account_id} className={`flex cursor-pointer items-center gap-3 border px-3 py-2 text-sm transition-colors ${checked ? 'border-blue-500 bg-blue-50' : 'border-gray-200 hover:border-gray-300'}`}>
                          <input
                            type="checkbox"
                            checked={checked}
                            onChange={(e) => setManualAccountIds(previous => e.target.checked ? [...previous, account.account_id] : previous.filter(id => id !== account.account_id))}
                            className="h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                          />
                          <span className="min-w-0"><span className="block truncate font-medium text-gray-800">{account.remote_name}</span><span className="block text-xs text-gray-500">账号 ID {account.account_id}</span></span>
                        </label>
                      );
                    })}
                  </div>
                )}
              </fieldset>
            )}

            {/* Transfer mode */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-2">传输模式</label>
              <div className="flex gap-2">
                {[
                  { value: 'move', label: '移动', desc: '传输后删除源文件' },
                  { value: 'copy', label: '复制', desc: '保留源文件' },
                  { value: 'sync', label: '同步', desc: '目标与源一致，删除多余文件' },
                ].filter(mode => !isManualTask || mode.value !== 'sync').map(mode => (
                  <button
                    key={mode.value}
                    type="button"
                    aria-pressed={form.transfer_mode === mode.value}
                    onClick={() => handleChange('transfer_mode', mode.value)}
                    disabled={isManualTask && mode.value === 'sync'}
                    className={`flex-1 flex flex-col items-start p-3 rounded-lg border-2 text-left transition-colors ${
                      form.transfer_mode === mode.value
                        ? 'border-blue-500 bg-blue-50'
                        : 'border-gray-200 hover:border-gray-300'
                    } ${isManualTask && mode.value === 'sync' ? 'opacity-50 cursor-not-allowed' : ''}`}
                  >
                    <div className="font-medium text-sm">{mode.label}</div>
                    <div className="text-xs text-gray-500 mt-0.5">{mode.desc}</div>
                  </button>
                ))}
              </div>
            </div>

            {/* Source section */}
            <div className="border-t pt-4">
              <div className="grid grid-cols-1 md:grid-cols-[170px_minmax(0,1fr)] gap-3 items-start">
                <div>
                  <div className="text-sm font-medium text-gray-700 mb-2">源目录</div>
                  <div className="inline-flex bg-gray-100 rounded-lg p-0.5">
                    <button
                      type="button"
                      onClick={() => handleChange('source_type', 'local')}
                      disabled={isManualTask}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${
                        (isManualTask ? 'local' : form.source_type) === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                    <button
                      type="button"
                      onClick={() => handleChange('source_type', 'remote')}
                      disabled={isManualTask}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${
                        (isManualTask ? 'local' : form.source_type) === 'remote' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <Cloud className="w-3 h-3 inline mr-1" />云盘
                    </button>
                  </div>
                </div>

                <div className="min-w-0">
                  {(isManualTask ? 'local' : form.source_type) === 'local' ? (
                    <div>
                      <div className="text-xs text-gray-500 mb-1">本地路径</div>
                      <input
                        type="text"
                        required
                        value={form.source_dir}
                        onChange={(e) => handleChange('source_dir', e.target.value)}
                        placeholder="/home/media"
                        className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                      />
                    </div>
                  ) : (
                    <div className="space-y-2">
                      <div className="grid grid-cols-1 md:grid-cols-[220px_minmax(0,1fr)_92px] gap-2 items-start">
                        <div>
                          <div className="text-xs text-gray-500 mb-1">远程盘符</div>
                          <div className="relative">
                            <select
                              required
                              value={parseRemotePath(form.source_dir).remote}
                              onChange={(e) => updateSourceRemote(e.target.value)}
                              className="w-full h-10 px-3 pr-9 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 appearance-none"
                            >
                              <option value="">选择远程盘符</option>
                              {remotes.map(remote => (
                                <option key={remote} value={remote}>{remote}</option>
                              ))}
                            </select>
                            <Cloud className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400 pointer-events-none" />
                          </div>
                        </div>
                        <div>
                          <div className="text-xs text-gray-500 mb-1">云盘路径</div>
                          <input
                            type="text"
                            required
                            value={parseRemotePath(form.source_dir).path}
                            onChange={(e) => updateSourceRemotePath(e.target.value)}
                            placeholder="/videos"
                            className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                          />
                        </div>
                        <div>
                          <div className="hidden md:block text-xs text-transparent mb-1">操作</div>
                          <button
                            type="button"
                            onClick={() => openBrowser('source')}
                            className="w-full h-10 px-3 bg-gray-100 hover:bg-gray-200 rounded-lg text-sm font-medium transition-colors flex items-center justify-center gap-1"
                          >
                            <Folder className="w-4 h-4" /> 浏览
                          </button>
                        </div>
                      </div>
                      {remotes.length === 0 && (
                        <p className="text-xs text-orange-500">未检测到 rclone 配置，请确保配置文件已挂载</p>
                      )}
                      <p className="text-xs text-gray-400">
                        保存时会自动组合为 rclone 路径：{form.source_dir || 'remote:/path'}
                      </p>
                    </div>
                  )}
                </div>
              </div>
            </div>

            {/* Destination section */}
            <div className="border-t pt-4">
              <div className="grid grid-cols-1 md:grid-cols-[170px_minmax(0,1fr)] gap-3 items-start">
                <div>
                  <div className="text-sm font-medium text-gray-700 mb-2">目标目录</div>
                  <div className="inline-flex bg-gray-100 rounded-lg p-0.5">
                    <button
                      type="button"
                      onClick={() => handleChange('dest_type', 'remote')}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.dest_type === 'remote' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <Cloud className="w-3 h-3 inline mr-1" />云盘
                    </button>
                    <button
                      type="button"
                      onClick={() => !isManualTask && handleChange('dest_type', 'local')}
                      disabled={isManualTask}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.dest_type === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      } ${isManualTask ? 'opacity-50 cursor-not-allowed' : ''}`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                  </div>
                  {isManualTask && (
                    <p className="text-xs text-amber-600 mt-2">手动分配任务目标固定为云盘。</p>
                  )}
                </div>

                <div className="min-w-0">
                  {form.dest_type === 'remote' ? (
                    <div className="space-y-2">
                      <div className="grid grid-cols-1 md:grid-cols-[220px_minmax(0,1fr)_92px] gap-2 items-start">
                        <div>
                          <div className="text-xs text-gray-500 mb-1">远程盘符</div>
                          <div className="relative">
                              <select
                                required
                                value={form.remote_name}
                                onChange={(e) => handleChange('remote_name', e.target.value)}
                                className="w-full h-10 px-3 pr-9 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 appearance-none"
                              >
                                <option value="">选择远程盘符</option>
                                {remotes.map(remote => (
                                  <option key={remote} value={remote}>{remote}</option>
                                ))}
                              </select>
                              <Cloud className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400 pointer-events-none" />
                            </div>
                          </div>
                         <div>
                          <div className="text-xs text-gray-500 mb-1">云盘路径</div>
                          <input
                            type="text"
                            required
                            value={form.remote_dir}
                            onChange={(e) => handleChange('remote_dir', e.target.value)}
                            placeholder="/media"
                            className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                          />
                        </div>
                        <div>
                          <div className="hidden md:block text-xs text-transparent mb-1">操作</div>
                          <button
                            type="button"
                            onClick={() => openBrowser('dest')}
                            className="w-full h-10 px-3 bg-gray-100 hover:bg-gray-200 rounded-lg text-sm font-medium transition-colors flex items-center justify-center gap-1"
                          >
                            <Folder className="w-4 h-4" /> 浏览
                          </button>
                        </div>
                      </div>
                      {remotes.length === 0 && (
                        <p className="text-xs text-orange-500">未检测到 rclone 配置，请确保配置文件已挂载</p>
                      )}
                    </div>
                  ) : (
                    <div>
                      <div className="text-xs text-gray-500 mb-1">本地路径</div>
                      <input
                        type="text"
                        required
                        value={form.remote_dir}
                        onChange={(e) => handleChange('remote_dir', e.target.value)}
                        placeholder="/backup/media"
                        className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                      />
                    </div>
                  )}
                </div>
              </div>
            </div>

          </div>
        </div>

        {/* Performance */}
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
            <Settings2 className="w-5 h-5 text-purple-500" />
            性能配置
          </h2>

          <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">并发传输数</label>
              <input
                type="number"
                min="1"
                max="64"
                value={form.transfers}
                onChange={(e) => handleChange('transfers', parseInt(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">并发检查数</label>
              <input
                type="number"
                min="1"
                max="128"
                value={form.checkers}
                onChange={(e) => handleChange('checkers', parseInt(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">最小文件年龄</label>
              <input
                type="text"
                value={form.min_age}
                onChange={(e) => handleChange('min_age', e.target.value)}
                placeholder="10s"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">上传块大小</label>
              <input
                type="text"
                value={form.drive_chunk_size}
                onChange={(e) => handleChange('drive_chunk_size', e.target.value)}
                placeholder="256M"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">缓冲区大小</label>
              <input
                type="text"
                value={form.buffer_size}
                onChange={(e) => handleChange('buffer_size', e.target.value)}
                placeholder="512M"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">重试次数</label>
              <input
                type="number"
                min="0"
                max="10"
                value={form.retries}
                onChange={(e) => handleChange('retries', parseInt(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">绑定 IP (可选)</label>
              <input
                type="text"
                value={form.bind_ip}
                onChange={(e) => handleChange('bind_ip', e.target.value)}
                placeholder="IPv4 或 IPv6 地址"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">自定义配置路径 (可选)</label>
              <input
                type="text"
                value={form.rclone_config}
                onChange={(e) => handleChange('rclone_config', e.target.value)}
                placeholder="/path/to/rclone.conf"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
              />
            </div>
          </div>
        </div>

        {!isManualTask && (
        <>
        {/* Automation */}
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
            <Clock className="w-5 h-5 text-green-500" />
            自动化设置
          </h2>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">目录监控</div>
                <div className="text-sm text-gray-500">源目录有文件变化时自动触发传输</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.watch_enabled}
                  onChange={(e) => handleChange('watch_enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">定时执行</div>
                <div className="text-sm text-gray-500">按固定间隔自动执行传输任务</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.schedule_enabled}
                  onChange={(e) => handleChange('schedule_enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            {form.schedule_enabled && (
              <div className="md:ml-4 p-4 border-l-2 border-blue-200">
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  执行间隔（分钟）
                </label>
                <input
                  type="number"
                  min="1"
                  value={form.schedule_interval}
                  onChange={(e) => handleChange('schedule_interval', parseInt(e.target.value))}
                  className="w-32 px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                />
              </div>
            )}

            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">qBittorrent 完成触发</div>
                <div className="text-sm text-gray-500">种子完成后自动触发转移</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.qb_enabled}
                  onChange={(e) => handleChange('qb_enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            {form.qb_enabled && (
              <div className="md:ml-4 p-4 border-l-2 border-green-200 space-y-4">
                <p className="text-xs text-gray-500">
                  当前逻辑：轮询 qBittorrent，发现源目录下种子 100% 后只转移该种子的内容；多个完成种子会一个个排队处理；任务成功后删除该种子，默认不删除文件。
                </p>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">qBittorrent 地址 <span className="text-red-500">*</span></label>
                    <input
                      type="text"
                      value={form.qb_url}
                      onChange={(e) => handleChange('qb_url', e.target.value)}
                      placeholder="http://127.0.0.1:8080"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">轮询间隔（秒）</label>
                    <input
                      type="number"
                      min="10"
                      value={form.qb_poll_interval}
                      onChange={(e) => handleChange('qb_poll_interval', parseInt(e.target.value) || 60)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">用户名（可选）</label>
                    <input
                      type="text"
                      value={form.qb_username}
                      onChange={(e) => handleChange('qb_username', e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">密码（可选）</label>
                    <input
                      type="password"
                      value={form.qb_password}
                      onChange={(e) => handleChange('qb_password', e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                </div>
                <label className="flex items-start gap-2 text-sm text-gray-600 cursor-pointer">
                  <input
                    type="checkbox"
                    checked={form.qb_delete_files}
                    onChange={(e) => handleChange('qb_delete_files', e.target.checked)}
                    className="mt-0.5 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                  />
                  <span>删除种子时同时删除文件（默认关闭；如果 rclone 是 move，一般不需要开启）</span>
                </label>
              </div>
            )}

            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">启用任务</div>
                <div className="text-sm text-gray-500">禁用后不会自动触发任何操作</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(e) => handleChange('enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>
          </div>
        </div>
        </>
        )}



        {/* Actions */}
        <div className="flex items-center justify-end gap-3">
          <button
            type="button"
            onClick={() => navigate('/tasks')}
            className="px-4 py-2 text-gray-700 hover:bg-gray-100 rounded-lg transition-colors font-medium"
          >
            取消
          </button>
          <button
            type="submit"
            disabled={saving}
            className="inline-flex items-center gap-2 px-6 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium disabled:opacity-50"
          >
            <Save className="w-4 h-4" />
            {saving ? '保存中...' : (isEdit ? '保存修改' : '创建任务')}
          </button>
        </div>
      </form>


      {/* Directory Browser Modal */}
      {browserOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div className="absolute inset-0 bg-black/50" onClick={() => setBrowserOpen(false)} />
          <div className="relative bg-white rounded-xl shadow-xl border border-gray-200 w-full max-w-lg mx-4 max-h-[80vh] flex flex-col">
            {/* Header */}
            <div className="flex items-center justify-between p-4 border-b">
              <div>
                <h3 className="font-semibold text-gray-900">选择{browserTarget === 'source' ? '源' : '目标'}目录</h3>
                <p className="text-xs text-gray-500">{browserRemote}:</p>
              </div>
              <button
                type="button"
                onClick={() => setBrowserOpen(false)}
                className="p-1.5 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg"
              >
                <X className="w-5 h-5" />
              </button>
            </div>

            {/* Breadcrumbs */}
            <div className="flex items-center gap-1 px-4 py-2 border-b bg-gray-50 overflow-x-auto">
              {browserBreadcrumbs.map((crumb, i) => (
                <div key={i} className="flex items-center gap-1 shrink-0">
                  {i > 0 && <ChevronRight className="w-3 h-3 text-gray-400" />}
                  <button
                    type="button"
                    onClick={() => browserGoToCrumb(crumb)}
                    className={`text-xs px-1.5 py-0.5 rounded hover:bg-gray-200 transition-colors ${
                      i === browserBreadcrumbs.length - 1 ? 'text-blue-600 font-medium' : 'text-gray-600'
                    }`}
                  >
                    {i === 0 ? <Home className="w-3 h-3 inline" /> : crumb.name}
                  </button>
                </div>
              ))}
            </div>

            {/* Item list */}
            <div className="flex-1 overflow-y-auto p-2">
              {browserLoading ? (
                <div className="flex items-center justify-center py-8">
                  <div className="animate-spin rounded-full h-6 w-6 border-b-2 border-blue-600"></div>
                </div>
              ) : browserItems.length === 0 ? (
                <div className="text-center py-8 text-gray-400 text-sm">此目录为空</div>
              ) : (
                browserItems.map((item, i) => (
                  <button
                    key={i}
                    type="button"
                    onClick={() => browserNavigate(item)}
                    className="w-full flex items-center gap-3 px-3 py-2.5 rounded-lg hover:bg-gray-50 text-left transition-colors"
                  >
                    <Folder className="w-4 h-4 text-yellow-500 shrink-0" />
                    <span className="text-sm text-gray-800 truncate">{item.name}</span>
                  </button>
                ))
              )}
            </div>

            {/* Footer */}
            <div className="flex items-center justify-between p-4 border-t bg-gray-50">
              <span className="text-xs text-gray-500 truncate max-w-xs">
                {browserRemote}:{browserPath}
              </span>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => setBrowserOpen(false)}
                  className="px-4 py-2 text-gray-700 hover:bg-gray-100 rounded-lg text-sm font-medium"
                >
                  取消
                </button>
                <button
                  type="button"
                  onClick={browserSelect}
                  className="px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 text-sm font-medium"
                >
                  选择此目录
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default TaskForm;

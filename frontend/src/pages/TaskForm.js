import React, { useState, useEffect, useMemo, useCallback } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { 
  ArrowLeft, 
  Save, 
  FolderOpen, 
  Cloud, 
  Settings2,
  Clock,
  RefreshCw,
  Plus,
  X,
  Pencil,
  Trash2,
  Link2,
  Folder,
  ChevronRight,
  Home,
  HardDrive,
  ShieldCheck,
  KeyRound,
  ListChecks
} from 'lucide-react';
import { createTask, updateTask, getTask, getRemotes, listRemoteDir, getOpenlistConfigs } from '../services/api';
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
    rotation_remotes: '[]',
    rotation_strategy: 'legacy_error',
    rotation_quota_limit_bytes: 700 * 1024 * 1024 * 1024,
    rotation_quota_keys: '{}',
    rotation_max_rounds: 3,
    rotation_resume_time: '01:00',
    rotation_current_index: 0,
    rotation_current_round: 0,
    rotation_paused_until: null,
    transfer_mode: 'move',
    transfers: 16,
    checkers: 32,
    bind_ip: '',
    rclone_config: '',
    enabled: true,
    auto_dedupe: true,
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
    openlist_enabled: false,
    openlist_url: '',
    openlist_mapping: '',
    openlist_token: '',
    openlist_config_id: 0,
  });

  const [remotes, setRemotes] = useState([]);
  const [openlistConfigs, setOpenlistConfigs] = useState([]);
  const [loading, setLoading] = useState(isEdit);
  const [saving, setSaving] = useState(false);
  const [legacyRotationValues, setLegacyRotationValues] = useState(null);

  const [mapModalOpen, setMapModalOpen] = useState(false);
  const [mapEditIndex, setMapEditIndex] = useState(null);
  const [mapRclonePath, setMapRclonePath] = useState('');
  const [mapOpenlistPath, setMapOpenlistPath] = useState('');

  // Directory browser state
  const [browserOpen, setBrowserOpen] = useState(false);
  const [browserTarget, setBrowserTarget] = useState('source'); // 'source' or 'dest'
  const [browserRemote, setBrowserRemote] = useState('');
  const [browserPath, setBrowserPath] = useState('/');
  const [browserItems, setBrowserItems] = useState([]);
  const [browserLoading, setBrowserLoading] = useState(false);
  const [browserBreadcrumbs, setBrowserBreadcrumbs] = useState([{ name: '/', path: '/' }]);

  const isRotationTask = form.task_type === 'rotation';
  const isProactiveQuota = isRotationTask && form.rotation_strategy === 'proactive_quota';

  const parseRotationRemotes = useCallback((value = form.rotation_remotes) => {
    try {
      const parsed = JSON.parse(value || '[]');
      return Array.isArray(parsed) ? parsed.filter(Boolean) : [];
    } catch {
      return [];
    }
  }, [form.rotation_remotes]);

  const selectedRotationRemotes = useMemo(() => parseRotationRemotes(), [parseRotationRemotes]);

  const isValidResumeTime = useCallback((value) => /^([01]\d|2[0-3]):([0-5]\d)$/.test(value), []);

  const rotationLogicText = useMemo(() => {
    if (!isRotationTask) return null;
    const remotesText = selectedRotationRemotes.length > 0 ? selectedRotationRemotes.join(' → ') : 'A → B → C';
    const sourceText = form.source_dir || '/x';
    const destPath = form.remote_dir || '/目标目录';
    const rounds = form.rotation_max_rounds || 3;
    const resumeTime = form.rotation_resume_time || '01:00';
    return `当前逻辑：监控 ${sourceText} 后按 ${remotesText} 顺序上传到同一目标目录 ${destPath}；某个账号返回 403/429/限额后自动切到下一个；连续轮转 ${rounds} 轮后暂停，到 ${resumeTime} 自动从 ${selectedRotationRemotes[0] || 'A'} 重新开始。`;
  }, [isRotationTask, selectedRotationRemotes, form.source_dir, form.remote_dir, form.rotation_max_rounds, form.rotation_resume_time]);

  const loadRemotes = useCallback(async () => {
    try {
      const res = await getRemotes();
      setRemotes(res.data.remotes || []);
    } catch (err) {
      console.error('Failed to load remotes');
    }
  }, []);

  const loadOpenlistConfigs = useCallback(async () => {
    try {
      const res = await getOpenlistConfigs();
      setOpenlistConfigs(res.data || []);
    } catch (err) {
      console.error('Failed to load OpenList configs');
      setOpenlistConfigs([]);
    }
  }, []);

  const loadTask = useCallback(async () => {
    try {
      const res = await getTask(id);
      setForm({
        ...res.data,
        task_type: res.data.task_type || 'normal',
        rotation_remotes: res.data.rotation_remotes || '[]',
        rotation_strategy: res.data.rotation_strategy || 'legacy_error',
        rotation_quota_limit_bytes: res.data.rotation_quota_limit_bytes == null
          ? 700 * 1024 * 1024 * 1024
          : res.data.rotation_quota_limit_bytes,
        rotation_quota_keys: res.data.rotation_quota_keys || '{}',
        rotation_max_rounds: res.data.rotation_max_rounds || 3,
        rotation_resume_time: res.data.rotation_resume_time || '01:00',
        openlist_config_id: res.data.openlist_config_id || 0,
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
    loadOpenlistConfigs();
    if (isEdit) {
      loadTask();
    }
  }, [isEdit, loadRemotes, loadOpenlistConfigs, loadTask]);

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
      remote = isRotationTask ? selectedRotationRemotes[0] : form.remote_name;
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

    if (form.openlist_enabled && !form.openlist_config_id) {
      toast.error('启用 OpenList 刷新时，请选择一个 OpenList 配置');
      return;
    }

    if (form.qb_enabled && !(isRotationTask && form.rotation_strategy === 'proactive_quota')) {
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

    if (isRotationTask) {
      if (submitForm.rotation_strategy === 'proactive_quota') {
        submitForm = {
          ...submitForm,
          source_type: 'local',
          qb_enabled: false,
          qb_url: '',
          qb_username: '',
          qb_password: '',
          qb_poll_interval: 60,
          qb_delete_files: false,
        };
      }
      if (submitForm.dest_type !== 'remote') {
        toast.error('调度轮转任务的目标必须是云盘');
        return;
      }

      const normalizedRemotes = parseRotationRemotes(submitForm.rotation_remotes);
      if (normalizedRemotes.length === 0) {
        toast.error('调度轮转任务至少选择一个网盘');
        return;
      }

      if (!isValidResumeTime(submitForm.rotation_resume_time)) {
        toast.error('恢复时间格式应为 HH:MM');
        return;
      }

      if (submitForm.rotation_strategy === 'proactive_quota') {
        if (submitForm.source_type !== 'local') {
          toast.error('主动额度轮转只支持本地源目录');
          return;
        }
        if (submitForm.transfer_mode !== 'copy' && submitForm.transfer_mode !== 'move') {
          toast.error('主动额度轮转只支持复制或移动模式');
          return;
        }
        const quotaLimitBytes = Number(submitForm.rotation_quota_limit_bytes);
        const maxQuotaBytes = 700 * 1024 * 1024 * 1024;
        if (!Number.isFinite(quotaLimitBytes) || quotaLimitBytes < 0 || quotaLimitBytes > maxQuotaBytes) {
          toast.error('每账号每日额度必须在 0 到 700 GiB 之间');
          return;
        }
        try {
          const quotaKeys = JSON.parse(submitForm.rotation_quota_keys || '{}');
          const selected = new Set(normalizedRemotes);
          const unknown = Object.keys(quotaKeys).some(remote => !selected.has(remote));
          if (unknown || Object.values(quotaKeys).some(key => typeof key !== 'string' || !key.trim())) {
            toast.error('请检查账号池中的 quota key，且只能填写已选择的远程账号');
            return;
          }
        } catch {
          toast.error('账号池 quota key 配置无效');
          return;
        }
      }

      submitForm = {
        ...submitForm,
        rotation_remotes: JSON.stringify(normalizedRemotes),
        remote_name: normalizedRemotes[0],
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
      dest_type: taskType === 'rotation' ? 'remote' : prev.dest_type,
      rotation_remotes: taskType === 'rotation' && parseRotationRemotes(prev.rotation_remotes).length === 0 && prev.remote_name
        ? JSON.stringify([prev.remote_name])
        : prev.rotation_remotes,
      remote_name: taskType === 'rotation' ? (parseRotationRemotes(prev.rotation_remotes)[0] || prev.remote_name) : prev.remote_name,
    }));
  };

  const toggleRotationRemote = (remote) => {
    const current = parseRotationRemotes();
    const next = current.includes(remote)
      ? current.filter(item => item !== remote)
      : [...current, remote];

    setForm(prev => ({
      ...prev,
      rotation_remotes: JSON.stringify(next),
      remote_name: next[0] || '',
      rotation_quota_keys: JSON.stringify(next.reduce((keys, item) => {
        if (parseRotationQuotaKeys[item]) keys[item] = parseRotationQuotaKeys[item];
        return keys;
      }, {})),
    }));
  };

  const parseRotationQuotaKeys = useMemo(() => {
    try {
      const parsed = JSON.parse(form.rotation_quota_keys || '{}');
      return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {};
    } catch {
      return {};
    }
  }, [form.rotation_quota_keys]);

  const setRotationQuotaKey = (remote, value) => {
    const next = { ...parseRotationQuotaKeys };
    if (value.trim()) next[remote] = value;
    else delete next[remote];
    handleChange('rotation_quota_keys', JSON.stringify(next));
  };

  const handleRotationStrategyChange = (strategy) => {
    setForm(prev => {
      if (strategy === 'proactive_quota' && prev.rotation_strategy !== 'proactive_quota') {
        setLegacyRotationValues({
          source_type: prev.source_type,
          source_dir: prev.source_dir,
          transfer_mode: prev.transfer_mode,
          qb_enabled: prev.qb_enabled,
        });
      }
      if (strategy === 'legacy_error' && prev.rotation_strategy === 'proactive_quota' && legacyRotationValues) {
        setLegacyRotationValues(null);
        return { ...prev, rotation_strategy: strategy, ...legacyRotationValues };
      }
      return {
        ...prev,
        rotation_strategy: strategy,
        transfer_mode: strategy === 'proactive_quota' ? 'copy' : prev.transfer_mode,
      };
    });
  };

  const parseMappings = useMemo(() => {
    try {
      const obj = JSON.parse(form.openlist_mapping || '{}');
      return Object.entries(obj).map(([rclone, openlist]) => ({
        rclone,
        openlist,
      }));
    } catch {
      return [];
    }
  }, [form.openlist_mapping]);

  const syncMappingsToForm = (entries) => {
    const obj = {};
    entries.forEach(({ rclone, openlist }) => {
      if (rclone.trim() && openlist.trim()) {
        obj[rclone.trim()] = openlist.trim();
      }
    });
    handleChange('openlist_mapping', Object.keys(obj).length > 0 ? JSON.stringify(obj) : '');
  };

  const openAddMapping = () => {
    setMapEditIndex(null);
    setMapRclonePath('');
    setMapOpenlistPath('');
    setMapModalOpen(true);
  };

  const openEditMapping = (index) => {
    setMapEditIndex(index);
    setMapRclonePath(parseMappings[index].rclone);
    setMapOpenlistPath(parseMappings[index].openlist);
    setMapModalOpen(true);
  };

  const saveMapping = () => {
    if (!mapRclonePath.trim()) {
      toast.error('请输入 rclone 传输路径');
      return;
    }
    if (!mapOpenlistPath.trim()) {
      toast.error('请输入 OpenList 路径');
      return;
    }
    const entries = [...parseMappings];
    if (mapEditIndex !== null) {
      entries[mapEditIndex] = { rclone: mapRclonePath.trim(), openlist: mapOpenlistPath.trim() };
    } else {
      const exists = entries.findIndex(e => e.rclone === mapRclonePath.trim());
      if (exists >= 0 && exists !== mapEditIndex) {
        toast.error('该 rclone 传输路径已存在');
        return;
      }
      entries.push({ rclone: mapRclonePath.trim(), openlist: mapOpenlistPath.trim() });
    }
    syncMappingsToForm(entries);
    setMapModalOpen(false);
  };

  const deleteMapping = (index) => {
    const entries = parseMappings.filter((_, i) => i !== index);
    syncMappingsToForm(entries);
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
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
            {isEdit ? '修改现有任务配置' : '配置新的 Rclone 自动化任务'}
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
                  { value: 'rotation', label: '调度轮转任务', desc: '用于 Google Drive 多子账号顺序上传' },
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

            {/* Transfer mode */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-2">传输模式</label>
              <div className="flex gap-2">
                {[
                  { value: 'move', label: '移动', desc: '传输后删除源文件' },
                  { value: 'copy', label: '复制', desc: '保留源文件' },
                  { value: 'sync', label: '同步', desc: '目标与源一致，删除多余文件' },
                ].filter(mode => !isProactiveQuota || mode.value !== 'sync').map(mode => (
                  <button
                    key={mode.value}
                    type="button"
                    aria-pressed={form.transfer_mode === mode.value}
                    onClick={() => handleChange('transfer_mode', mode.value)}
                    disabled={isProactiveQuota && mode.value === 'sync'}
                    className={`flex-1 flex flex-col items-start p-3 rounded-lg border-2 text-left transition-colors ${
                      form.transfer_mode === mode.value
                        ? 'border-blue-500 bg-blue-50'
                        : 'border-gray-200 hover:border-gray-300'
                    } ${isProactiveQuota && mode.value === 'sync' ? 'opacity-50 cursor-not-allowed' : ''}`}
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
                      disabled={isRotationTask && form.rotation_strategy === 'proactive_quota'}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${
                        (isProactiveQuota ? 'local' : form.source_type) === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                    <button
                      type="button"
                      onClick={() => handleChange('source_type', 'remote')}
                      disabled={isRotationTask && form.rotation_strategy === 'proactive_quota'}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${
                        (isProactiveQuota ? 'local' : form.source_type) === 'remote' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <Cloud className="w-3 h-3 inline mr-1" />云盘
                    </button>
                  </div>
                </div>

                <div className="min-w-0">
                  {(isProactiveQuota ? 'local' : form.source_type) === 'local' ? (
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
                      onClick={() => !isRotationTask && handleChange('dest_type', 'local')}
                      disabled={isRotationTask}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.dest_type === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      } ${isRotationTask ? 'opacity-50 cursor-not-allowed' : ''}`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                  </div>
                  {isRotationTask && (
                    <p className="text-xs text-amber-600 mt-2">调度轮转任务目标固定为云盘，避免误配置到本地目录。</p>
                  )}
                </div>

                <div className="min-w-0">
                  {form.dest_type === 'remote' ? (
                    <div className="space-y-2">
                      <div className="grid grid-cols-1 md:grid-cols-[220px_minmax(0,1fr)_92px] gap-2 items-start">
                        <div>
                          <div className="text-xs text-gray-500 mb-1">{isRotationTask ? '轮转网盘' : '远程盘符'}</div>
                          {isRotationTask ? (
                            <div className="border border-gray-300 rounded-lg p-2 max-h-40 overflow-y-auto space-y-1 bg-white">
                              {remotes.length === 0 ? (
                                <div className="text-xs text-gray-400">暂无可选网盘</div>
                              ) : (
                                remotes.map(remote => (
                                  <label key={remote} className="flex items-center gap-2 px-2 py-1.5 rounded hover:bg-gray-50 cursor-pointer text-sm text-gray-700">
                                    <input
                                      type="checkbox"
                                      checked={selectedRotationRemotes.includes(remote)}
                                      onChange={() => toggleRotationRemote(remote)}
                                      className="rounded border-gray-300 text-blue-600 focus:ring-blue-500"
                                    />
                                    <span>{remote}</span>
                                  </label>
                                ))
                              )}
                            </div>
                          ) : (
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
                          )}
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
                      {isRotationTask && (
                        <div className="grid grid-cols-1 md:grid-cols-2 gap-4 p-3 bg-blue-50 border border-blue-100 rounded-lg">
                          <div>
                            <label className="block text-xs text-gray-500 mb-1">最大轮数</label>
                            <input
                              type="number"
                              min="1"
                              value={form.rotation_max_rounds}
                              onChange={(e) => handleChange('rotation_max_rounds', parseInt(e.target.value) || 1)}
                              className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                            />
                          </div>
                          <div>
                            <label className="block text-xs text-gray-500 mb-1">恢复时间</label>
                            <input
                              type="time"
                              value={form.rotation_resume_time}
                              onChange={(e) => handleChange('rotation_resume_time', e.target.value)}
                              className="w-full h-10 px-3 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                            />
                          </div>
                        </div>
                      )}
                      {remotes.length === 0 && (
                        <p className="text-xs text-orange-500">未检测到 rclone 配置，请确保配置文件已挂载</p>
                      )}
                      {isRotationTask && rotationLogicText && (
                        <p className="text-xs text-blue-700 bg-blue-50 border border-blue-100 rounded-lg px-3 py-2">{rotationLogicText}</p>
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

            {isRotationTask && (
              <div className="border-t pt-4">
                <div className="flex items-start gap-3 mb-3">
                  <ShieldCheck className="w-5 h-5 text-emerald-600 mt-0.5" />
                  <div>
                    <h3 className="text-sm font-semibold text-gray-900">轮转策略</h3>
                    <p className="text-xs text-gray-500 mt-1 max-w-2xl">
                      选择何时切换账号。已有的异常触发轮转保持不变；主动额度池会在上传前按账号配额预留批次。
                    </p>
                  </div>
                </div>

                <div className="grid grid-cols-1 md:grid-cols-2 gap-2">
                  {[
                    { value: 'legacy_error', label: '异常触发轮转', desc: '遇到 403、429 或额度错误后切换账号' },
                    { value: 'proactive_quota', label: '主动额度账号池', desc: '按每日额度预留和排队，减少中途失败' },
                  ].map(strategy => (
                    <button
                      key={strategy.value}
                      type="button"
                      onClick={() => handleRotationStrategyChange(strategy.value)}
                      aria-pressed={form.rotation_strategy === strategy.value}
                      className={`p-3 rounded-lg border-2 text-left transition-colors focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-1 ${
                        form.rotation_strategy === strategy.value
                          ? 'border-blue-500 bg-blue-50'
                          : 'border-gray-200 hover:border-gray-300'
                      }`}
                    >
                      <div className="font-medium text-sm text-gray-900">{strategy.label}</div>
                      <div className="text-xs text-gray-500 mt-1">{strategy.desc}</div>
                    </button>
                  ))}
                </div>

                {form.rotation_strategy === 'proactive_quota' && (
                  <div className="mt-3 rounded-lg border border-emerald-200 bg-emerald-50/70 p-4 space-y-4">
                    <div className="flex items-start gap-2 text-sm text-emerald-900">
                      <ListChecks className="w-4 h-4 mt-0.5 shrink-0" />
                      <p>
                        主动额度池的安全边界：每个账号每天最多 <strong>700 GiB</strong>，按额度预留批次并使用复制模式保留源文件。
                      </p>
                    </div>

                    <div>
                      <label htmlFor="rotation-quota-limit" className="block text-xs font-medium text-gray-700 mb-1">每账号每日额度</label>
                      <div className="flex flex-col sm:flex-row sm:items-center gap-2 min-w-0">
                        <input
                          id="rotation-quota-limit"
                          type="number"
                          min="0"
                          max="700"
                          step="0.01"
                          value={(Number(form.rotation_quota_limit_bytes || 0) / (1024 * 1024 * 1024)).toFixed(2).replace(/\.00$/, '')}
                          onChange={(e) => {
                            const gib = Number(e.target.value);
                            handleChange('rotation_quota_limit_bytes', Number.isFinite(gib) ? Math.round(gib * 1024 * 1024 * 1024) : '');
                          }}
                          aria-describedby="quota-limit-help"
                          className="w-36 h-10 px-3 border border-emerald-300 bg-white rounded-lg text-sm font-medium text-gray-800 focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                        />
                        <span className="text-sm text-gray-700">GiB / 天</span>
                        <span id="quota-limit-help" className="text-xs text-gray-600">默认 700；可降低，不能超过 700 GiB。服务端会再次校验。</span>
                      </div>
                    </div>

                    <fieldset>
                      <legend className="text-xs font-medium text-gray-700 mb-2">账号池与 quota key（可选）</legend>
                      {selectedRotationRemotes.length === 0 ? (
                        <p className="text-xs text-amber-700 bg-amber-100 rounded-md px-3 py-2">先在上方选择至少一个轮转网盘，再为账号补充 quota key。</p>
                      ) : (
                        <div className="space-y-2">
                          {selectedRotationRemotes.map((remote, index) => (
                            <div key={remote} className="grid grid-cols-[auto_minmax(0,1fr)] md:grid-cols-[auto_180px_minmax(0,1fr)] items-center gap-2 bg-white border border-emerald-100 rounded-md p-2 min-w-0">
                              <span className="text-xs font-mono text-gray-500 w-6">{String(index + 1).padStart(2, '0')}</span>
                              <div className="flex items-center gap-2 min-w-0">
                                <Cloud className="w-4 h-4 text-blue-500 shrink-0" />
                                <span className="text-sm font-medium text-gray-800 truncate" title={remote}>{remote}</span>
                              </div>
                              <label className="relative block md:col-auto col-span-2 md:col-span-1 min-w-0 w-full">
                                <span className="sr-only">{remote} 的 quota key</span>
                                <KeyRound className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
                                <input
                                  type="text"
                                  value={parseRotationQuotaKeys[remote] || ''}
                                  onChange={(e) => setRotationQuotaKey(remote, e.target.value)}
                                  placeholder="quota key（可选）"
                                  className="w-full h-9 pl-9 pr-3 border border-gray-300 rounded-md text-sm focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                                />
                              </label>
                            </div>
                          ))}
                        </div>
                      )}
                      <p className="text-xs text-gray-500 mt-2">不填写时会根据配置身份和远程名生成稳定的默认 key；填写后会将该 key 绑定到对应账号。key 不会显示在任务列表中。</p>
                    </fieldset>

                    <div className="grid grid-cols-1 md:grid-cols-3 gap-2 text-xs text-gray-700">
                      <div className="bg-white/80 rounded-md px-3 py-2"><strong>队列</strong><br />按文件快照生成批次</div>
                      <div className="bg-white/80 rounded-md px-3 py-2"><strong>额度</strong><br />预留后再启动传输</div>
                      <div className="bg-white/80 rounded-md px-3 py-2"><strong>恢复</strong><br />达到额度后等待下一窗口</div>
                    </div>
                    <p className="text-xs text-emerald-800">账号池任务使用额度预留与批次执行，不使用 qBittorrent 完成触发。</p>
                  </div>
                )}
              </div>
            )}
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
                <div className="font-medium text-gray-900">自动去重</div>
                <div className="text-sm text-gray-500">传输完成后自动执行 dedupe newest</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.auto_dedupe}
                  onChange={(e) => handleChange('auto_dedupe', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
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

            {!isProactiveQuota && <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">qBittorrent 完成触发</div>
                <div className="text-sm text-gray-500">种子 100% 完成后只转移该种子的内容，成功后删除种子</div>
              </div>
              <label className={`relative inline-flex items-center cursor-pointer ${isRotationTask && form.rotation_strategy === 'proactive_quota' ? 'opacity-50 cursor-not-allowed' : ''}`}>
                <input
                  type="checkbox"
                  checked={isProactiveQuota ? false : form.qb_enabled}
                  onChange={(e) => handleChange('qb_enabled', e.target.checked)}
                  disabled={isRotationTask && form.rotation_strategy === 'proactive_quota'}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer peer-disabled:cursor-not-allowed peer-disabled:opacity-60 after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>}

            {!isProactiveQuota && form.qb_enabled && (
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

        {/* OpenList Refresh */}
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
            <RefreshCw className="w-5 h-5 text-orange-500" />
            OpenList 刷新设置
          </h2>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">启用 OpenList 刷新</div>
                <div className="text-sm text-gray-500">转移成功后自动刷新 OpenList 目录缓存</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.openlist_enabled}
                  onChange={(e) => handleChange('openlist_enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            {form.openlist_enabled && (
              <div className="md:ml-4 p-4 border-l-2 border-orange-200 space-y-4">
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    OpenList 配置 <span className="text-red-500">*</span>
                  </label>
                  <select
                    required={form.openlist_enabled}
                    value={form.openlist_config_id || ''}
                    onChange={(e) => handleChange('openlist_config_id', e.target.value ? Number(e.target.value) : 0)}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  >
                    <option value="">选择已有 OpenList 配置</option>
                    {openlistConfigs.map((cfg) => (
                      <option key={cfg.id} value={cfg.id}>{cfg.name}</option>
                    ))}
                  </select>
                  {openlistConfigs.length === 0 && (
                    <button
                      type="button"
                      onClick={() => navigate('/openlist-configs')}
                      className="text-xs text-blue-600 hover:text-blue-700 mt-1"
                    >
                      暂无配置，前往添加
                    </button>
                  )}
                </div>

                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    路径映射（可选）
                  </label>
                  <p className="text-xs text-gray-500 mb-2">
                    当 OpenList 挂载路径与 rclone 目标路径不一致时，通过映射刷新正确的目录
                  </p>

                  {parseMappings.length > 0 && (
                    <div className="mb-2 space-y-1.5">
                      {parseMappings.map((m, i) => (
                        <div key={i} className="flex items-center gap-2 bg-gray-50 border border-gray-200 rounded-lg px-3 py-2">
                          <Link2 className="w-3.5 h-3.5 text-gray-400 shrink-0" />
                          <code className="text-xs font-mono text-blue-700 bg-blue-50 px-1.5 py-0.5 rounded">{m.rclone}</code>
                          <span className="text-xs text-gray-400">→</span>
                          <code className="text-xs font-mono text-green-700 bg-green-50 px-1.5 py-0.5 rounded">{m.openlist}</code>
                          <div className="flex-1" />
                          <button
                            type="button"
                            onClick={() => openEditMapping(i)}
                            className="p-1 text-gray-400 hover:text-blue-500 hover:bg-gray-100 rounded transition-colors"
                          >
                            <Pencil className="w-3.5 h-3.5" />
                          </button>
                          <button
                            type="button"
                            onClick={() => deleteMapping(i)}
                            className="p-1 text-gray-400 hover:text-red-500 hover:bg-gray-100 rounded transition-colors"
                          >
                            <Trash2 className="w-3.5 h-3.5" />
                          </button>
                        </div>
                      ))}
                    </div>
                  )}

                  <button
                    type="button"
                    onClick={openAddMapping}
                    className="inline-flex items-center gap-1.5 px-3 py-2 text-sm border border-dashed border-gray-300 rounded-lg text-gray-600 hover:border-blue-400 hover:text-blue-600 hover:bg-blue-50 transition-colors"
                  >
                    <Plus className="w-4 h-4" />
                    {parseMappings.length === 0 ? '添加路径映射' : '添加更多映射'}
                  </button>
                </div>

              </div>
            )}
          </div>
        </div>

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

      {/* Path Mapping Modal */}
      {mapModalOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div className="absolute inset-0 bg-black/50" onClick={() => setMapModalOpen(false)} />
          <div className="relative bg-white rounded-xl shadow-xl border border-gray-200 w-full max-w-md mx-4 p-6">
            <div className="flex items-center justify-between mb-5">
              <h3 className="text-lg font-semibold text-gray-900">
                {mapEditIndex !== null ? '编辑路径映射' : '添加路径映射'}
              </h3>
              <button
                type="button"
                onClick={() => setMapModalOpen(false)}
                className="p-1.5 text-gray-400 hover:text-gray-600 hover:bg-gray-100 rounded-lg transition-colors"
              >
                <X className="w-5 h-5" />
              </button>
            </div>

            <div className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1.5">
                  rclone 传输路径 <span className="text-red-500">*</span>
                </label>
                <input
                  type="text"
                  value={mapRclonePath}
                  onChange={(e) => setMapRclonePath(e.target.value)}
                  placeholder="例如：op:s1"
                  autoFocus
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                />
              </div>

              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1.5">
                  OpenList 路径 <span className="text-red-500">*</span>
                </label>
                <input
                  type="text"
                  value={mapOpenlistPath}
                  onChange={(e) => setMapOpenlistPath(e.target.value)}
                  placeholder="例如：/s2"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                />
                <p className="text-xs text-gray-400 mt-1">OpenList 中对应的挂载路径，刷新时将用此路径调用 API</p>
              </div>
            </div>

            <div className="flex items-center justify-end gap-2 mt-6">
              <button
                type="button"
                onClick={() => setMapModalOpen(false)}
                className="px-4 py-2 text-gray-700 hover:bg-gray-100 rounded-lg transition-colors font-medium text-sm"
              >
                取消
              </button>
              <button
                type="button"
                onClick={saveMapping}
                className="px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium text-sm"
              >
                {mapEditIndex !== null ? '保存修改' : '添加映射'}
              </button>
            </div>
          </div>
        </div>
      )}
      
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

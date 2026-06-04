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
  HardDrive
} from 'lucide-react';
import { createTask, updateTask, getTask, getRemotes, listRemoteDir, getOpenlistConfigs } from '../services/api';
import toast from 'react-hot-toast';

const TaskForm = () => {
  const { id } = useParams();
  const navigate = useNavigate();
  const isEdit = !!id;

  const [form, setForm] = useState({
    name: '',
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
    auto_dedupe: true,
    min_age: '10s',
    drive_chunk_size: '256M',
    buffer_size: '512M',
    retries: 3,
    schedule_enabled: false,
    schedule_interval: 15,
    watch_enabled: true,
    interface_log_enabled: false,
    interface_log_url: '',
    interface_log_token: '',
    interface_log_interval: 30,
    interface_log_match_path: '',
    completion_action: '',
    smartstrm_webhook_url: '',
    smartstrm_task_name: '',
    smartstrm_path_mapping: '',
    smartstrm_delay: 0,
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
      setForm({ ...res.data, openlist_config_id: res.data.openlist_config_id || 0 });
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

    if (form.openlist_enabled && !form.openlist_config_id) {
      toast.error('启用 OpenList 刷新时，请选择一个 OpenList 配置');
      return;
    }

    setSaving(true);

    try {
      if (isEdit) {
        await updateTask(id, form);
        toast.success('任务已更新');
      } else {
        await createTask(form);
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

            {/* Transfer mode */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-2">传输模式</label>
              <div className="flex gap-2">
                {[
                  { value: 'move', label: '移动', desc: '传输后删除源文件' },
                  { value: 'copy', label: '复制', desc: '保留源文件' },
                  { value: 'sync', label: '同步', desc: '目标与源一致，删除多余文件' },
                ].map(mode => (
                  <button
                    key={mode.value}
                    type="button"
                    onClick={() => handleChange('transfer_mode', mode.value)}
                    className={`flex-1 flex flex-col items-start p-3 rounded-lg border-2 text-left transition-colors ${
                      form.transfer_mode === mode.value
                        ? 'border-blue-500 bg-blue-50'
                        : 'border-gray-200 hover:border-gray-300'
                    }`}
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
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.source_type === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                    <button
                      type="button"
                      onClick={() => handleChange('source_type', 'remote')}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.source_type === 'remote' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <Cloud className="w-3 h-3 inline mr-1" />云盘
                    </button>
                  </div>
                </div>

                <div className="min-w-0">
                  {form.source_type === 'local' ? (
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
                      onClick={() => handleChange('dest_type', 'local')}
                      className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                        form.dest_type === 'local' ? 'bg-white shadow text-blue-600' : 'text-gray-500'
                      }`}
                    >
                      <HardDrive className="w-3 h-3 inline mr-1" />本地
                    </button>
                  </div>
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
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">接口日志监听</div>
                <div className="text-sm text-gray-500">监听 MoviePilot 转移历史，有新记录时自动触发传输</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.interface_log_enabled}
                  onChange={(e) => handleChange('interface_log_enabled', e.target.checked)}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            {form.interface_log_enabled && (
              <div className="md:ml-4 p-4 border-l-2 border-blue-200 space-y-4">
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    接口地址 <span className="text-red-500">*</span>
                  </label>
                  <input
                    type="url"
                    required={form.interface_log_enabled}
                    value={form.interface_log_url}
                    onChange={(e) => handleChange('interface_log_url', e.target.value)}
                    placeholder="http://moviepilot:3001 或完整 /api/v1/history/transfer 地址"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                  />
                  <p className="text-xs text-gray-400 mt-1">参考 openlist-refresh：默认轮询 /api/v1/history/transfer?page=1&count=50。</p>
                </div>

                <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
                  <div className="md:col-span-2">
                    <label className="block text-sm font-medium text-gray-700 mb-1">接口 Token</label>
                    <input
                      type="password"
                      autoComplete="new-password"
                      value={form.interface_log_token}
                      onChange={(e) => handleChange('interface_log_token', e.target.value)}
                      placeholder="MoviePilot token，可留空并写在 URL query 中"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">轮询间隔（秒）</label>
                    <input
                      type="number"
                      min="5"
                      value={form.interface_log_interval}
                      onChange={(e) => handleChange('interface_log_interval', parseInt(e.target.value) || 30)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                </div>

                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">匹配路径前缀（可选）</label>
                  <input
                    type="text"
                    value={form.interface_log_match_path}
                    onChange={(e) => handleChange('interface_log_match_path', e.target.value)}
                    placeholder="留空时使用源目录作为前缀，例如 /opt/media/国产剧"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                  />
                  <p className="text-xs text-gray-400 mt-1">新日志的 file_path/dest/src 命中此前缀才会触发当前任务；首次启用只建立基线，不会处理旧日志。</p>
                </div>
              </div>
            )}

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

        {/* Completion Actions */}
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
            <RefreshCw className="w-5 h-5 text-cyan-500" />
            传输完成动作
          </h2>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
              <div>
                <div className="font-medium text-gray-900">通知 SmartStrm 生成 STRM</div>
                <div className="text-sm text-gray-500">rclone 传输成功后发送 SmartStrm webhook</div>
              </div>
              <label className="relative inline-flex items-center cursor-pointer">
                <input
                  type="checkbox"
                  checked={form.completion_action === 'smartstrm'}
                  onChange={(e) => handleChange('completion_action', e.target.checked ? 'smartstrm' : '')}
                  className="sr-only peer"
                />
                <div className="w-11 h-6 bg-gray-200 peer-focus:ring-4 peer-focus:ring-blue-300 rounded-full peer peer-checked:after:translate-x-full peer-checked:after:border-white after:content-[''] after:absolute after:top-[2px] after:left-[2px] after:bg-white after:border-gray-300 after:border after:rounded-full after:h-5 after:w-5 after:transition-all peer-checked:bg-blue-600"></div>
              </label>
            </div>

            {form.completion_action === 'smartstrm' && (
              <div className="md:ml-4 p-4 border-l-2 border-cyan-200 space-y-4">
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    SmartStrm webhook URL <span className="text-red-500">*</span>
                  </label>
                  <input
                    type="url"
                    required={form.completion_action === 'smartstrm'}
                    value={form.smartstrm_webhook_url}
                    onChange={(e) => handleChange('smartstrm_webhook_url', e.target.value)}
                    placeholder="http://smartstrm:端口/api/webhook"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                  />
                </div>

                <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
                  <div className="md:col-span-2">
                    <label className="block text-sm font-medium text-gray-700 mb-1">
                      SmartStrm 任务名 <span className="text-red-500">*</span>
                    </label>
                    <input
                      type="text"
                      required={form.completion_action === 'smartstrm'}
                      value={form.smartstrm_task_name}
                      onChange={(e) => handleChange('smartstrm_task_name', e.target.value)}
                      placeholder="例如 s2"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">延迟（秒）</label>
                    <input
                      type="number"
                      min="0"
                      value={form.smartstrm_delay}
                      onChange={(e) => handleChange('smartstrm_delay', parseInt(e.target.value) || 0)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                </div>

                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">SmartStrm 路径映射（可选 JSON）</label>
                  <textarea
                    rows="3"
                    value={form.smartstrm_path_mapping}
                    onChange={(e) => handleChange('smartstrm_path_mapping', e.target.value)}
                    placeholder='{"op:/media":"/s2/media"}'
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                  />
                  <p className="text-xs text-gray-400 mt-1">系统会把传输后的目标路径映射成 SmartStrm 的 storage_path，并自动取文件所在目录。</p>
                </div>
              </div>
            )}
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

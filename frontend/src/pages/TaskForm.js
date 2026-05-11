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
import { createTask, updateTask, getTask, getRemotes, listRemoteDir } from '../services/api';
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
    openlist_enabled: false,
    openlist_url: '',
    openlist_mapping: '',
    openlist_token: '',
  });

  const [remotes, setRemotes] = useState([]);
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

  const loadTask = useCallback(async () => {
    try {
      const res = await getTask(id);
      setForm(res.data);
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

  // Directory browser functions
  const parseRemotePath = (input) => {
    if (!input) return { remote: '', path: '/' };
    const idx = input.indexOf(':');
    if (idx > 0) {
      return { remote: input.substring(0, idx), path: input.substring(idx + 1) || '/' };
    }
    return { remote: '', path: '/' };
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
      toast.error('请输入 rclone 路径前缀');
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
        toast.error('该 rclone 路径前缀已存在');
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
                    className={`flex-1 p-3 rounded-lg border-2 text-left transition-colors ${
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
              <div className="flex items-center gap-3 mb-3">
                <span className="text-sm font-medium text-gray-700">源目录</span>
                <div className="flex bg-gray-100 rounded-lg p-0.5">
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

              {form.source_type === 'local' ? (
                <input
                  type="text"
                  required
                  value={form.source_dir}
                  onChange={(e) => handleChange('source_dir', e.target.value)}
                  placeholder="/home/media"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              ) : (
                <div className="flex gap-2">
                  <input
                    type="text"
                    required
                    value={form.source_dir}
                    onChange={(e) => handleChange('source_dir', e.target.value)}
                    placeholder="op:/videos (盘符:路径)"
                    className="flex-1 px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                  <button
                    type="button"
                    onClick={() => openBrowser('source')}
                    className="px-3 py-2 bg-gray-100 hover:bg-gray-200 rounded-lg text-sm font-medium transition-colors flex items-center gap-1"
                  >
                    <Folder className="w-4 h-4" /> 浏览
                  </button>
                </div>
              )}
            </div>

            {/* Destination section */}
            <div className="border-t pt-4">
              <div className="flex items-center gap-3 mb-3">
                <span className="text-sm font-medium text-gray-700">目标目录</span>
                <div className="flex bg-gray-100 rounded-lg p-0.5">
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

              {form.dest_type === 'remote' ? (
                <div className="space-y-3">
                  <div className="relative">
                    <select
                      required
                      value={form.remote_name}
                      onChange={(e) => handleChange('remote_name', e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 appearance-none"
                    >
                      <option value="">选择远程盘符</option>
                      {remotes.map(remote => (
                        <option key={remote} value={remote}>{remote}</option>
                      ))}
                    </select>
                    <Cloud className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400 pointer-events-none" />
                  </div>
                  {remotes.length === 0 && (
                    <p className="text-xs text-orange-500">未检测到 rclone 配置，请确保配置文件已挂载</p>
                  )}
                  <div className="flex gap-2">
                    <input
                      type="text"
                      required
                      value={form.remote_dir}
                      onChange={(e) => handleChange('remote_dir', e.target.value)}
                      placeholder="media"
                      className="flex-1 px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                    />
                    <button
                      type="button"
                      onClick={() => openBrowser('dest')}
                      className="px-3 py-2 bg-gray-100 hover:bg-gray-200 rounded-lg text-sm font-medium transition-colors flex items-center gap-1"
                    >
                      <Folder className="w-4 h-4" /> 浏览
                    </button>
                  </div>
                </div>
              ) : (
                <input
                  type="text"
                  required
                  value={form.remote_dir}
                  onChange={(e) => handleChange('remote_dir', e.target.value)}
                  placeholder="/backup/media"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              )}
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
                    OpenList 地址 <span className="text-red-500">*</span>
                  </label>
                  <input
                    type="text"
                    required={form.openlist_enabled}
                    value={form.openlist_url}
                    onChange={(e) => handleChange('openlist_url', e.target.value)}
                    placeholder="http://localhost:5244"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                  />
                  <p className="text-xs text-gray-500 mt-1">
                    填写 OpenList 的基础地址，程序会自动调用 /api/fs/list 接口刷新目录
                  </p>
                  <p className="text-xs text-gray-400 mt-0.5">
                    例如：http://localhost:5244 或 https://your-domain.com
                  </p>
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

                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    认证 Token <span className="text-red-500">*</span>
                  </label>
                  <input
                    type="text"
                    required={form.openlist_enabled}
                    value={form.openlist_token}
                    onChange={(e) => handleChange('openlist_token', e.target.value)}
                    placeholder="openlist-xxx..."
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                  />
                  <p className="text-xs text-gray-500 mt-1">
                    OpenList API 认证 Token，用于调用 /api/fs/list 接口
                  </p>
                  <p className="text-xs text-gray-400 mt-0.5">
                    例如：openlist-4de0432a-f847-43a7-b6ef-f5d06ac7cbbf...（从 OpenList 管理后台获取）
                  </p>
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
                  rclone 路径前缀 <span className="text-red-500">*</span>
                </label>
                <input
                  type="text"
                  value={mapRclonePath}
                  onChange={(e) => setMapRclonePath(e.target.value)}
                  placeholder="例如：op:s1"
                  autoFocus
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 font-mono text-sm"
                />
                <p className="text-xs text-gray-400 mt-1">rclone remote 名称，用于匹配目标路径前缀</p>
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

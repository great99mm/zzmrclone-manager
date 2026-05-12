import React, { useEffect, useMemo, useState } from 'react';
import {
  Cloud,
  HardDrive,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Save,
  Settings2,
  Square,
  Trash2,
  X,
} from 'lucide-react';
import {
  createMount,
  deleteMount,
  getMountSystemInfo,
  getMounts,
  getRemotes,
  startMount,
  stopMount,
  updateMount,
} from '../services/api';
import toast from 'react-hot-toast';

const buildEmptyForm = () => ({
  name: '',
  remote_name: '',
  remote_path: '/',
  mount_path: '',
  rclone_config: '',
  enabled: false,
  allow_other: true,
  read_only: false,
  vfs_cache_mode: 'writes',
  dir_cache_time: '5m',
  poll_interval: '1m',
  uid: 0,
  gid: 0,
  extra_args: '',
});

const StatusBadge = ({ status }) => {
  const styles = {
    mounted: 'bg-emerald-100 text-emerald-700 border-emerald-200',
    starting: 'bg-amber-100 text-amber-700 border-amber-200',
    stopping: 'bg-orange-100 text-orange-700 border-orange-200',
    error: 'bg-red-100 text-red-700 border-red-200',
    stopped: 'bg-gray-100 text-gray-700 border-gray-200',
  };

  const labels = {
    mounted: '已挂载',
    starting: '挂载中',
    stopping: '卸载中',
    error: '异常',
    stopped: '未挂载',
  };

  return (
    <span className={`inline-flex items-center px-2.5 py-1 rounded-full text-xs font-medium border ${styles[status] || styles.stopped}`}>
      {labels[status] || '未挂载'}
    </span>
  );
};

const SwitchField = ({ checked, onChange, title, desc }) => (
  <label className="flex items-start gap-3 rounded-lg border border-gray-200 p-3 cursor-pointer hover:border-blue-200 hover:bg-blue-50/40 transition-colors">
    <input
      type="checkbox"
      checked={checked}
      onChange={(e) => onChange(e.target.checked)}
      className="mt-1 h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
    />
    <div>
      <div className="text-sm font-medium text-gray-900">{title}</div>
      <div className="text-xs text-gray-500 mt-0.5">{desc}</div>
    </div>
  </label>
);

const Mounts = () => {
  const [mounts, setMounts] = useState([]);
  const [remotes, setRemotes] = useState([]);
  const [systemInfo, setSystemInfo] = useState({ mount_root: '', notes: [] });
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState(buildEmptyForm());
  const [saving, setSaving] = useState(false);
  const [actionId, setActionId] = useState(null);

  const loadData = async (silent = false) => {
    if (silent) {
      setRefreshing(true);
    }
    try {
      const [mountsRes, systemRes, remotesRes] = await Promise.all([
        getMounts(),
        getMountSystemInfo(),
        getRemotes(),
      ]);
      setMounts(mountsRes.data || []);
      setSystemInfo(systemRes.data || { mount_root: '', notes: [] });
      setRemotes((remotesRes.data || {}).remotes || []);
    } catch (err) {
      if (!silent) {
        toast.error(err.response?.data?.error || '加载挂载配置失败');
      }
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
    const timer = setInterval(() => loadData(true), 5000);
    return () => clearInterval(timer);
  }, []);

  const mountRoot = systemInfo.mount_root || '';

  const configSummary = useMemo(() => {
    if (!editing) return [];
    return [
      ['挂载名称', form.name || '-'],
      ['远程路径', `${form.remote_name || '-'}:${form.remote_path || '/'}`],
      ['本地挂载目录', form.mount_path || '-'],
      ['配置文件', form.rclone_config || '/root/.config/rclone/rclone.conf'],
    ];
  }, [editing, form]);

  const sortedMounts = useMemo(
    () => [...mounts].sort((a, b) => new Date(b.created_at || 0) - new Date(a.created_at || 0)),
    [mounts]
  );

  const openAdd = () => {
    setEditing(null);
    setForm({ ...buildEmptyForm(), remote_name: remotes[0] || '' });
    setModalOpen(true);
  };

  const openEdit = (mount) => {
    setEditing(mount);
    setForm({
      name: mount.name || '',
      remote_name: mount.remote_name || '',
      remote_path: mount.remote_path || '/',
      mount_path: mount.mount_path || '',
      rclone_config: mount.rclone_config || '',
      enabled: !!mount.enabled,
      allow_other: !!mount.allow_other,
      read_only: !!mount.read_only,
      vfs_cache_mode: mount.vfs_cache_mode || 'writes',
      dir_cache_time: mount.dir_cache_time || '5m',
      poll_interval: mount.poll_interval || '1m',
      uid: mount.uid ?? 0,
      gid: mount.gid ?? 0,
      extra_args: mount.extra_args || '',
    });
    setModalOpen(true);
  };

  const closeModal = () => {
    setModalOpen(false);
    setEditing(null);
    setForm(buildEmptyForm());
  };

  const handleChange = (field, value) => {
    setForm((prev) => ({ ...prev, [field]: value }));
  };

  const handleSave = async () => {
    if (!form.name.trim()) {
      toast.error('请填写挂载名称');
      return;
    }
    if (!form.remote_name) {
      toast.error('请选择远程盘符');
      return;
    }

    const payload = {
      ...form,
      remote_path: form.remote_path || '/',
      mount_path: form.mount_path.trim(),
      uid: Number(form.uid || 0),
      gid: Number(form.gid || 0),
    };

    setSaving(true);
    try {
      if (editing) {
        await updateMount(editing.id, payload);
        toast.success('挂载配置已更新');
      } else {
        await createMount(payload);
        toast.success('挂载配置已创建');
      }
      closeModal();
      loadData(true);
    } catch (err) {
      toast.error(err.response?.data?.error || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const handleStart = async (mount) => {
    setActionId(mount.id);
    try {
      await startMount(mount.id);
      toast.success(`已挂载：${mount.name}`);
      loadData(true);
    } catch (err) {
      toast.error(err.response?.data?.error || '挂载失败');
    } finally {
      setActionId(null);
    }
  };

  const handleStop = async (mount) => {
    setActionId(mount.id);
    try {
      await stopMount(mount.id);
      toast.success(`已卸载：${mount.name}`);
      loadData(true);
    } catch (err) {
      toast.error(err.response?.data?.error || '卸载失败');
    } finally {
      setActionId(null);
    }
  };

  const handleDelete = async (mount) => {
    if (!window.confirm(`确定删除挂载「${mount.name}」吗？`)) return;
    setActionId(mount.id);
    try {
      await deleteMount(mount.id);
      toast.success('挂载配置已删除');
      loadData(true);
    } catch (err) {
      toast.error(err.response?.data?.error || '删除失败');
    } finally {
      setActionId(null);
    }
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600" />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col lg:flex-row lg:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">云盘挂载</h1>
          <p className="text-gray-500 mt-1">管理 rclone mount 本地挂载配置。</p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => loadData(true)}
            className="inline-flex items-center gap-2 px-4 py-2 border border-gray-200 bg-white text-gray-700 rounded-lg hover:bg-gray-50 font-medium"
          >
            <RefreshCw className={`w-4 h-4 ${refreshing ? 'animate-spin' : ''}`} /> 刷新
          </button>
          <button
            onClick={openAdd}
            className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 font-medium"
          >
            <Plus className="w-4 h-4" /> 新建挂载
          </button>
        </div>
      </div>

      {!systemInfo.supported && (
        <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-5 overflow-hidden">
          <div className="flex items-center gap-2 font-semibold text-sm tracking-wide uppercase text-sky-300">
            <Settings2 className="w-4 h-4" /> Docker 建议配置
          </div>
          <pre className="mt-4 text-xs leading-6 overflow-auto whitespace-pre-wrap break-all text-gray-700 bg-gray-50 border border-gray-200 rounded-lg p-3">{`devices:\n  - /dev/fuse:/dev/fuse\ncap_add:\n  - SYS_ADMIN\nsecurity_opt:\n  - apparmor:unconfined\nvolumes:\n  - type: bind\n    source: /host/path\n    target: /container/path\n    bind:\n      propagation: rshared`}</pre>
        </div>
      )}

      {sortedMounts.length === 0 ? (
        <div className="bg-white rounded-xl border border-gray-200 shadow-sm p-12 text-center">
          <div className="w-16 h-16 rounded-xl bg-blue-50 flex items-center justify-center mx-auto">
            <Cloud className="w-8 h-8 text-sky-600" />
          </div>
          <h3 className="mt-4 text-lg font-semibold text-gray-900">还没有挂载配置</h3>
          <p className="mt-2 text-sm text-gray-500 max-w-xl mx-auto">
            创建挂载配置后，可在文件浏览器或任务中使用本地挂载目录。
          </p>
        </div>
      ) : (
        <div className="grid grid-cols-1 2xl:grid-cols-2 gap-4">
          {sortedMounts.map((mount) => {
            const busy = actionId === mount.id;
            return (
              <div key={mount.id} className="bg-white rounded-xl border border-gray-200 shadow-sm overflow-hidden">
                <div className="px-5 py-4 border-b border-gray-100 bg-white">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2 flex-wrap">
                        <h3 className="text-lg font-semibold text-gray-900 truncate">{mount.name}</h3>
                        <StatusBadge status={mount.status} />
                      </div>
                      <div className="text-xs text-gray-500 mt-1">ID: {mount.id}</div>
                    </div>
                    <div className="w-11 h-11 rounded-xl bg-blue-50 flex items-center justify-center shrink-0">
                      <Cloud className="w-5 h-5 text-sky-700" />
                    </div>
                  </div>
                </div>

                <div className="p-5 space-y-4">
                  <div className="rounded-lg border border-gray-200 bg-gray-50 p-4 space-y-3">
                    <div className="flex items-start gap-2 text-sm text-gray-700">
                      <Cloud className="w-4 h-4 mt-0.5 text-sky-600 shrink-0" />
                      <span className="break-all font-mono">{mount.remote_name}:{mount.remote_path || '/'}</span>
                    </div>
                    <div className="flex items-start gap-2 text-sm text-gray-700">
                      <HardDrive className="w-4 h-4 mt-0.5 text-blue-600 shrink-0" />
                      <span className="break-all font-mono">{mount.mount_path}</span>
                    </div>
                  </div>

                  <div className="flex flex-wrap gap-2 text-xs">
                    <span className="px-2.5 py-1 rounded-full bg-sky-100 text-sky-700">缓存 {mount.vfs_cache_mode || 'writes'}</span>
                    <span className="px-2.5 py-1 rounded-full bg-gray-100 text-gray-700">轮询 {mount.poll_interval || '1m'}</span>
                    <span className="px-2.5 py-1 rounded-full bg-gray-100 text-gray-700">目录缓存 {mount.dir_cache_time || '5m'}</span>
                    {mount.enabled && <span className="px-2.5 py-1 rounded-full bg-emerald-100 text-emerald-700">容器启动自动挂载</span>}
                    {mount.allow_other && <span className="px-2.5 py-1 rounded-full bg-violet-100 text-violet-700">allow_other</span>}
                    {mount.read_only && <span className="px-2.5 py-1 rounded-full bg-amber-100 text-amber-700">只读</span>}
                  </div>

                  <div className="grid grid-cols-2 gap-3 text-sm">
                    <div className="rounded-lg border border-gray-200 p-3">
                      <div className="text-gray-500">UID / GID</div>
                      <div className="font-semibold text-gray-900 mt-1">{mount.uid ?? 0} / {mount.gid ?? 0}</div>
                    </div>
                    <div className="rounded-lg border border-gray-200 p-3">
                      <div className="text-gray-500">最近挂载</div>
                      <div className="font-semibold text-gray-900 mt-1 break-all text-xs">
                        {mount.last_mounted_at ? new Date(mount.last_mounted_at).toLocaleString() : '尚未挂载'}
                      </div>
                    </div>
                  </div>

                  {mount.last_error && mount.status === 'error' && (
                    <div className="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 break-all">
                      {mount.last_error}
                    </div>
                  )}

                  <div className="flex flex-wrap items-center gap-2 pt-1">
                    {mount.status === 'mounted' || mount.status === 'starting' ? (
                      <button
                        onClick={() => handleStop(mount)}
                        disabled={busy}
                        className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-red-600 text-white hover:bg-red-700 font-medium disabled:opacity-50"
                      >
                        <Square className="w-4 h-4" /> {busy ? '处理中...' : '卸载'}
                      </button>
                    ) : (
                      <button
                        onClick={() => handleStart(mount)}
                        disabled={busy || !systemInfo.supported}
                        className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-blue-600 text-white hover:bg-blue-700 font-medium disabled:opacity-50"
                      >
                        <Play className="w-4 h-4" /> {busy ? '处理中...' : '挂载'}
                      </button>
                    )}
                    <button
                      onClick={() => openEdit(mount)}
                      disabled={busy}
                      className="inline-flex items-center gap-2 px-4 py-2 rounded-lg border border-gray-200 text-gray-700 hover:bg-gray-50 font-medium disabled:opacity-50"
                    >
                      <Pencil className="w-4 h-4" /> 编辑
                    </button>
                    <button
                      onClick={() => handleDelete(mount)}
                      disabled={busy}
                      className="inline-flex items-center gap-2 px-4 py-2 rounded-lg border border-red-200 text-red-600 hover:bg-red-50 font-medium disabled:opacity-50"
                    >
                      <Trash2 className="w-4 h-4" /> 删除
                    </button>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {modalOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
          <div className="absolute inset-0 bg-black/45" onClick={closeModal} />
          <div className="relative w-full max-w-4xl bg-white rounded-xl shadow-xl border border-gray-200 overflow-hidden max-h-[90vh] flex flex-col">
            <div className="px-6 py-5 border-b border-gray-100 flex items-center justify-between gap-4">
              <div>
                <div className="text-xl font-semibold text-gray-900">{editing ? '编辑挂载' : '新建挂载'}</div>
                <div className="text-sm text-gray-500 mt-1">填写容器内完整挂载路径。</div>
              </div>
              <button onClick={closeModal} className="p-2 rounded-lg text-gray-400 hover:text-gray-700 hover:bg-gray-100">
                <X className="w-5 h-5" />
              </button>
            </div>

            <div className="flex-1 overflow-auto px-6 py-5 space-y-6">
              {editing ? (
                <div className="bg-gray-50 border border-gray-200 rounded-lg p-4">
                  <div className="text-sm font-semibold text-gray-900 mb-3">配置概览</div>
                  <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                    {configSummary.map(([label, value]) => (
                      <div key={label}>
                        <div className="text-xs text-gray-500 mb-1">{label}</div>
                        <div className="text-sm font-mono text-gray-800 break-all bg-white border border-gray-200 rounded-lg px-3 py-2">{value}</div>
                      </div>
                    ))}
                  </div>
                </div>
              ) : (
                <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">挂载名称</label>
                    <input
                      value={form.name}
                      onChange={(e) => handleChange('name', e.target.value)}
                      placeholder="例如：影音库"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">远程盘符</label>
                    <select
                      value={form.remote_name}
                      onChange={(e) => handleChange('remote_name', e.target.value)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 bg-white"
                    >
                      <option value="">请选择远程盘符</option>
                      {remotes.map((remote) => (
                        <option key={remote} value={remote}>{remote}</option>
                      ))}
                    </select>
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">远程路径</label>
                    <input
                      value={form.remote_path}
                      onChange={(e) => handleChange('remote_path', e.target.value)}
                      placeholder="/ 或 /movies"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono"
                    />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">本地挂载目录</label>
                    <input
                      value={form.mount_path}
                      onChange={(e) => handleChange('mount_path', e.target.value)}
                      placeholder={mountRoot ? `${mountRoot}/movies` : '/data/cloud-movies'}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono"
                    />
                  </div>
                  <div className="lg:col-span-2">
                    <label className="block text-sm font-medium text-gray-700 mb-1">指定 rclone 配置文件</label>
                    <input
                      value={form.rclone_config}
                      onChange={(e) => handleChange('rclone_config', e.target.value)}
                      placeholder="默认使用 /root/.config/rclone/rclone.conf"
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono"
                    />
                  </div>
                </div>
              )}

              <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
                <SwitchField
                  checked={form.enabled}
                  onChange={(value) => handleChange('enabled', value)}
                  title="容器启动自动挂载"
                  desc="容器启动后自动挂载。"
                />
                <SwitchField
                  checked={form.allow_other}
                  onChange={(value) => handleChange('allow_other', value)}
                  title="allow_other"
                  desc="允许容器内其它进程访问挂载目录。"
                />
                <SwitchField
                  checked={form.read_only}
                  onChange={(value) => handleChange('read_only', value)}
                  title="只读挂载"
                  desc="适合纯浏览/播放，不允许写入远端。"
                />
              </div>

              <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">缓存模式</label>
                  <select
                    value={form.vfs_cache_mode}
                    onChange={(e) => handleChange('vfs_cache_mode', e.target.value)}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 bg-white"
                  >
                    <option value="off">off</option>
                    <option value="minimal">minimal</option>
                    <option value="writes">writes</option>
                    <option value="full">full</option>
                  </select>
                </div>
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">目录缓存时间</label>
                  <input
                    value={form.dir_cache_time}
                    onChange={(e) => handleChange('dir_cache_time', e.target.value)}
                    placeholder="5m"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">轮询间隔</label>
                  <input
                    value={form.poll_interval}
                    onChange={(e) => handleChange('poll_interval', e.target.value)}
                    placeholder="1m"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">UID</label>
                  <input
                    type="number"
                    min="0"
                    value={form.uid}
                    onChange={(e) => handleChange('uid', e.target.value)}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">GID</label>
                  <input
                    type="number"
                    min="0"
                    value={form.gid}
                    onChange={(e) => handleChange('gid', e.target.value)}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                  />
                </div>
                <div className="lg:col-span-3">
                  <label className="block text-sm font-medium text-gray-700 mb-1">额外挂载参数</label>
                  <textarea
                    rows={3}
                    value={form.extra_args}
                    onChange={(e) => handleChange('extra_args', e.target.value)}
                    placeholder="例如：--buffer-size 16M --vfs-read-chunk-size 32M"
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                  />
                  <p className="text-xs text-gray-500 mt-1">按空格分隔参数，会直接追加到 rclone mount 命令后面。</p>
                </div>
              </div>
            </div>

            <div className="px-6 py-4 border-t border-gray-100 bg-gray-50 flex items-center justify-end gap-3">
              <button
                onClick={closeModal}
                className="px-4 py-2 rounded-lg border border-gray-200 text-gray-700 hover:bg-white font-medium"
              >
                取消
              </button>
              <button
                onClick={handleSave}
                disabled={saving}
                className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-blue-600 text-white hover:bg-blue-700 font-medium disabled:opacity-50"
              >
                <Save className="w-4 h-4" /> {saving ? '保存中...' : '保存配置'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default Mounts;

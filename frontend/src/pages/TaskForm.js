import React, { useState, useEffect } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { 
  ArrowLeft, 
  Save, 
  FolderOpen, 
  Cloud, 
  Settings2,
  Clock,
  Eye,
  CheckCircle2
} from 'lucide-react';
import { createTask, updateTask, getTask, getRemotes } from '../services/api';
import toast from 'react-hot-toast';

const TaskForm = () => {
  const { id } = useParams();
  const navigate = useNavigate();
  const isEdit = !!id;

  const [form, setForm] = useState({
    name: '',
    source_dir: '',
    remote_name: '',
    remote_dir: '',
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
  });

  const [remotes, setRemotes] = useState([]);
  const [loading, setLoading] = useState(isEdit);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    loadRemotes();
    if (isEdit) {
      loadTask();
    }
  }, []);

  const loadRemotes = async () => {
    try {
      const res = await getRemotes();
      setRemotes(res.data.remotes || []);
    } catch (err) {
      console.error('Failed to load remotes');
    }
  };

  const loadTask = async () => {
    try {
      const res = await getTask(id);
      setForm(res.data);
    } catch (err) {
      toast.error('加载任务失败');
      navigate('/tasks');
    } finally {
      setLoading(false);
    }
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

          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
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
              <label className="block text-sm font-medium text-gray-700 mb-1">
                本地源目录 <span className="text-red-500">*</span>
              </label>
              <input
                type="text"
                required
                value={form.source_dir}
                onChange={(e) => handleChange('source_dir', e.target.value)}
                placeholder="/home/media"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">
                远程盘符 <span className="text-red-500">*</span>
              </label>
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
                <p className="text-xs text-orange-500 mt-1">未检测到 rclone 配置，请确保配置文件已挂载</p>
              )}
            </div>

            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">
                远程目录 <span className="text-red-500">*</span>
              </label>
              <input
                type="text"
                required
                value={form.remote_dir}
                onChange={(e) => handleChange('remote_dir', e.target.value)}
                placeholder="media"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
              />
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
    </div>
  );
};

export default TaskForm;

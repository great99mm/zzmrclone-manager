import React, { useEffect, useState } from 'react';
import { Plus, Save, Trash2, Pencil, X, Server } from 'lucide-react';
import {
  getOpenlistConfigs,
  createOpenlistConfig,
  updateOpenlistConfig,
  deleteOpenlistConfig,
} from '../services/api';
import toast from 'react-hot-toast';

const emptyForm = { name: '', url: '', token: '' };

const OpenlistConfigs = () => {
  const [configs, setConfigs] = useState([]);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState(emptyForm);
  const [saving, setSaving] = useState(false);

  const loadConfigs = async () => {
    try {
      const res = await getOpenlistConfigs();
      setConfigs(res.data || []);
    } catch (err) {
      toast.error('加载 OpenList 配置失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadConfigs();
  }, []);

  const openAdd = () => {
    setEditing(null);
    setForm(emptyForm);
    setModalOpen(true);
  };

  const openEdit = (cfg) => {
    setEditing(cfg);
    setForm({ name: cfg.name || '', url: cfg.url || '', token: cfg.token || '' });
    setModalOpen(true);
  };

  const saveConfig = async () => {
    if (!form.name.trim() || !form.url.trim()) {
      toast.error('配置名和 OpenList 地址不能为空');
      return;
    }
    setSaving(true);
    try {
      if (editing) {
        await updateOpenlistConfig(editing.id, form);
        toast.success('配置已更新');
      } else {
        await createOpenlistConfig(form);
        toast.success('配置已添加');
      }
      setModalOpen(false);
      loadConfigs();
    } catch (err) {
      toast.error(err.response?.data?.error || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const removeConfig = async (cfg) => {
    if (!window.confirm(`确定删除「${cfg.name}」吗？`)) return;
    try {
      await deleteOpenlistConfig(cfg.id);
      toast.success('配置已删除');
      loadConfigs();
    } catch (err) {
      toast.error('删除失败');
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
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">OpenList 配置</h1>
        </div>
        <button
          onClick={openAdd}
          className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 font-medium"
        >
          <Plus className="w-4 h-4" /> 添加配置
        </button>
      </div>

      {configs.length === 0 ? (
        <div className="bg-white rounded-xl border border-gray-200 p-10 text-center text-gray-500">
          暂无 OpenList 配置，点击右上角添加
        </div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
          {configs.map((cfg) => (
            <div key={cfg.id} className="bg-white rounded-xl shadow-sm border border-gray-200 p-5">
              <div className="flex items-start gap-3">
                <div className="w-11 h-11 rounded-xl bg-blue-50 flex items-center justify-center shrink-0">
                  <Server className="w-5 h-5 text-blue-600" />
                </div>
                <div className="min-w-0 flex-1">
                  <div className="font-semibold text-gray-900 truncate">{cfg.name}</div>
                  <div className="text-sm text-gray-500 mt-1 break-all">{cfg.url}</div>
                  <div className="text-xs text-gray-400 mt-2">Token：{cfg.token ? '已配置' : '未配置'}</div>
                </div>
              </div>
              <div className="flex justify-end gap-2 mt-4">
                <button
                  onClick={() => openEdit(cfg)}
                  className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm border border-gray-200 rounded-lg hover:bg-gray-50"
                >
                  <Pencil className="w-3.5 h-3.5" /> 编辑
                </button>
                <button
                  onClick={() => removeConfig(cfg)}
                  className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm border border-red-200 text-red-600 rounded-lg hover:bg-red-50"
                >
                  <Trash2 className="w-3.5 h-3.5" /> 删除
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {modalOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div className="absolute inset-0 bg-black/50" onClick={() => setModalOpen(false)} />
          <div className="relative bg-white rounded-xl shadow-xl border border-gray-200 w-full max-w-md mx-4 p-6">
            <div className="flex items-center justify-between mb-5">
              <h3 className="text-lg font-semibold text-gray-900">{editing ? '编辑配置' : '添加配置'}</h3>
              <button onClick={() => setModalOpen(false)} className="p-1.5 text-gray-400 hover:text-gray-600">
                <X className="w-5 h-5" />
              </button>
            </div>

            <div className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">配置名</label>
                <input
                  value={form.name}
                  onChange={(e) => setForm((prev) => ({ ...prev, name: e.target.value }))}
                  placeholder="例如：家庭 OpenList"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">OpenList 地址</label>
                <input
                  value={form.url}
                  onChange={(e) => setForm((prev) => ({ ...prev, url: e.target.value }))}
                  placeholder="https://example.com"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">Token</label>
                <input
                  value={form.token}
                  onChange={(e) => setForm((prev) => ({ ...prev, token: e.target.value }))}
                  placeholder="openlist-xxx"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 font-mono text-sm"
                />
              </div>
              <button
                onClick={saveConfig}
                disabled={saving}
                className="w-full inline-flex items-center justify-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 font-medium disabled:opacity-50"
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

export default OpenlistConfigs;

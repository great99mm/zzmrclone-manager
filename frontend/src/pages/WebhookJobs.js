import React, { useEffect, useMemo, useState } from 'react';
import { RefreshCw, RotateCcw, Send, ShieldCheck, Webhook } from 'lucide-react';
import toast from 'react-hot-toast';
import { createWebhookJob, getWebhookConfig, getWebhookJob, getWebhookJobs, retryWebhookJob } from '../services/api';

const statusStyles = {
  pending: 'bg-slate-100 text-slate-700',
  running: 'bg-blue-100 text-blue-700',
  copying: 'bg-indigo-100 text-indigo-700',
  checking: 'bg-purple-100 text-purple-700',
  notifying_callback: 'bg-cyan-100 text-cyan-700',
  calling_curl_url: 'bg-cyan-100 text-cyan-700',
  success: 'bg-green-100 text-green-700',
  failed: 'bg-red-100 text-red-700',
};

const WebhookJobs = () => {
  const [jobs, setJobs] = useState([]);
  const [selectedJob, setSelectedJob] = useState(null);
  const [config, setConfig] = useState(null);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [form, setForm] = useState({
    path: '',
    callback_url: '',
    curl_url: '',
  });

  const webhookEndpoint = useMemo(() => `${window.location.origin}/webhook`, []);

  useEffect(() => {
    loadData();
    const interval = setInterval(loadJobs, 5000);
    return () => clearInterval(interval);
  }, []);

  const loadData = async () => {
    setLoading(true);
    try {
      const [jobsRes, configRes] = await Promise.all([getWebhookJobs(), getWebhookConfig()]);
      setJobs(jobsRes.data.jobs || []);
      setConfig(configRes.data);
    } catch (err) {
      toast.error('加载 Webhook 下载数据失败');
    } finally {
      setLoading(false);
    }
  };

  const loadJobs = async () => {
    try {
      const res = await getWebhookJobs();
      setJobs(res.data.jobs || []);
    } catch (err) {
      // keep silent during polling
    }
  };

  const loadJob = async (id) => {
    try {
      const res = await getWebhookJob(id);
      setSelectedJob(res.data);
    } catch (err) {
      toast.error('查询任务失败');
    }
  };

  const submitJob = async (event) => {
    event.preventDefault();
    setSubmitting(true);
    try {
      const res = await createWebhookJob(form);
      toast.success(`任务已创建：${res.data.job_id}`);
      setForm({ path: '', callback_url: '', curl_url: '' });
      await loadJobs();
      await loadJob(res.data.job_id);
    } catch (err) {
      toast.error(err.response?.data?.error || '创建失败');
    } finally {
      setSubmitting(false);
    }
  };

  const retryJob = async (job) => {
    try {
      await retryWebhookJob(job.id);
      toast.success('已重新入队');
      await loadJobs();
      await loadJob(job.id);
    } catch (err) {
      toast.error(err.response?.data?.error || '重试失败');
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
    <div className="space-y-6 min-w-0">
      <div className="flex flex-col lg:flex-row lg:items-end justify-between gap-4">
        <div>
          <div className="inline-flex items-center gap-2 px-3 py-1 rounded-full bg-blue-50 text-blue-700 text-sm font-medium mb-3">
            <Webhook className="w-4 h-4" />
            Webhook API
          </div>
          <h1 className="text-2xl font-bold text-gray-900">Webhook 下载调度</h1>
          <p className="text-gray-500 mt-1">接收外部通知后后台执行 rclone copy + check，并完成 callback / curl_url 通知链路。</p>
        </div>
        <button
          onClick={loadData}
          className="inline-flex items-center justify-center gap-2 px-4 py-2 bg-white border border-gray-200 rounded-lg hover:bg-gray-50 text-gray-700 font-medium"
        >
          <RefreshCw className="w-4 h-4" />
          刷新
        </button>
      </div>

      <div className="grid grid-cols-1 xl:grid-cols-[420px,1fr] gap-6">
        <div className="space-y-6">
          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
            <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
              <Send className="w-5 h-5 text-blue-500" />
              创建任务
            </h2>
            <form onSubmit={submitJob} className="space-y-4">
              <Field label="远端路径">
                <input
                  value={form.path}
                  onChange={(event) => setForm((prev) => ({ ...prev, path: event.target.value }))}
                  placeholder="/remote/folder/a"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Callback URL">
                <input
                  value={form.callback_url}
                  onChange={(event) => setForm((prev) => ({ ...prev, callback_url: event.target.value }))}
                  placeholder="https://sender.example.com/download-finished"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <Field label="Curl URL">
                <input
                  value={form.curl_url}
                  onChange={(event) => setForm((prev) => ({ ...prev, curl_url: event.target.value }))}
                  placeholder="https://api.example.com/reload?path=/remote/folder/a"
                  required
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500"
                />
              </Field>
              <button
                type="submit"
                disabled={submitting}
                className="w-full inline-flex items-center justify-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 font-medium disabled:opacity-50"
              >
                <Send className="w-4 h-4" />
                {submitting ? '提交中...' : '提交任务'}
              </button>
            </form>
          </div>

          <div className="bg-slate-950 text-slate-100 rounded-xl shadow-sm border border-slate-800 p-6 overflow-hidden">
            <h2 className="text-lg font-semibold mb-4 flex items-center gap-2">
              <ShieldCheck className="w-5 h-5 text-emerald-300" />
              接入信息
            </h2>
            <Info label="Endpoint" value={webhookEndpoint} mono />
            <Info label="Remote" value={config?.rclone_remote || '未配置 RCLONE_MANAGER_WEBHOOK_RCLONE_REMOTE'} mono />
            <Info label="Local Base" value={config?.local_base_dir || '-'} mono />
            <Info label="Token" value={config?.token_required ? '需要 Bearer 或 X-Webhook-Token' : '未启用'} />
            <div className="mt-4 text-xs text-slate-400 leading-6">
              环境变量配置：remote、local base、workers、白名单、webhook token。callback/curl host 白名单为空时允许所有 HTTP(S) 主机。
            </div>
          </div>
        </div>

        <div className="space-y-6 min-w-0">
          <div className="bg-white rounded-xl shadow-sm border border-gray-200 overflow-hidden">
            <div className="px-5 py-4 border-b border-gray-200 flex items-center justify-between">
              <h2 className="text-lg font-semibold text-gray-900">最近任务</h2>
              <span className="text-sm text-gray-500">{jobs.length} 条</span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[760px]">
                <thead className="bg-gray-50 border-b border-gray-200">
                  <tr>
                    <th className="px-5 py-3 text-left text-xs font-semibold text-gray-500 uppercase">Job ID</th>
                    <th className="px-5 py-3 text-left text-xs font-semibold text-gray-500 uppercase">路径</th>
                    <th className="px-5 py-3 text-left text-xs font-semibold text-gray-500 uppercase">状态</th>
                    <th className="px-5 py-3 text-left text-xs font-semibold text-gray-500 uppercase">更新时间</th>
                    <th className="px-5 py-3 text-right text-xs font-semibold text-gray-500 uppercase">操作</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-100">
                  {jobs.length === 0 ? (
                    <tr>
                      <td colSpan="5" className="px-5 py-12 text-center text-gray-500">暂无任务</td>
                    </tr>
                  ) : jobs.map((job) => (
                    <tr key={job.id} className="hover:bg-gray-50">
                      <td className="px-5 py-4 font-mono text-xs text-gray-700">{job.id}</td>
                      <td className="px-5 py-4 text-sm text-gray-700 max-w-xs truncate">{job.remote_path}</td>
                      <td className="px-5 py-4"><StatusBadge status={job.status} /></td>
                      <td className="px-5 py-4 text-sm text-gray-500">{formatTime(job.updated_at)}</td>
                      <td className="px-5 py-4">
                        <div className="flex justify-end gap-2">
                          <button onClick={() => loadJob(job.id)} className="px-3 py-1.5 text-sm bg-gray-100 text-gray-700 rounded-lg hover:bg-gray-200">详情</button>
                          {job.status === 'failed' && (
                            <button onClick={() => retryJob(job)} className="px-3 py-1.5 text-sm bg-red-50 text-red-700 rounded-lg hover:bg-red-100 inline-flex items-center gap-1">
                              <RotateCcw className="w-3.5 h-3.5" /> 重试
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6 min-w-0">
            <h2 className="text-lg font-semibold text-gray-900 mb-4">任务详情</h2>
            {selectedJob ? (
              <pre className="bg-gray-950 text-gray-100 rounded-lg p-4 text-xs overflow-auto max-h-[460px]">{JSON.stringify(selectedJob, null, 2)}</pre>
            ) : (
              <div className="text-gray-500 text-sm">点击最近任务中的“详情”查看完整 job 数据、错误和 rclone 日志。</div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

const Field = ({ label, children }) => (
  <label className="block">
    <span className="block text-sm font-medium text-gray-700 mb-1">{label}</span>
    {children}
  </label>
);

const Info = ({ label, value, mono }) => (
  <div className="py-2 border-b border-slate-800 last:border-b-0">
    <div className="text-xs uppercase tracking-wide text-slate-500">{label}</div>
    <div className={`mt-1 text-sm break-all ${mono ? 'font-mono' : ''}`}>{value}</div>
  </div>
);

const StatusBadge = ({ status }) => (
  <span className={`px-2 py-1 rounded-full text-xs font-medium ${statusStyles[status] || 'bg-gray-100 text-gray-700'}`}>
    {status}
  </span>
);

const formatTime = (value) => {
  if (!value) return '-';
  return new Date(value).toLocaleString();
};

export default WebhookJobs;

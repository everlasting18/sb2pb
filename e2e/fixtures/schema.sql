-- Typical Supabase app schema (pattern from Supabase quickstarts) + edge cases for migration testing
create type public.post_status as enum ('draft', 'published', 'archived');

create table public.profiles (
  id uuid primary key references auth.users (id) on delete cascade,
  username text unique,
  full_name text,
  avatar_url text,           -- storage path or full public URL
  website text,
  birthday date,
  phone text,                -- contact number typed by the user; must not replace the sign-in phone from auth.users
  created_at timestamptz not null default now(),
  updated_at timestamptz
);

create table public.posts (
  id uuid primary key default gen_random_uuid(),
  author_id uuid not null references public.profiles (id) on delete cascade,
  title text not null,
  body text,
  status public.post_status not null default 'draft',
  tags text[] default '{}',
  metadata jsonb,
  views integer not null default 0,
  big_counter bigint,             -- edge case: values > 2^53
  rating numeric(3,1),
  is_featured boolean,            -- nullable boolean (tri-state)
  published_at timestamptz,
  created_at timestamptz not null default now(),
  constraint title_len check (char_length(title) between 3 and 200)
);
create index posts_author_idx on public.posts (author_id);
create index posts_published_idx on public.posts (published_at) where status = 'published';

create table public.comments (
  id bigserial primary key,
  post_id uuid not null references public.posts (id) on delete cascade,
  user_id uuid not null references auth.users (id) on delete cascade,
  content text not null,
  created_at timestamptz not null default now()
);

create table public.post_likes (
  post_id uuid references public.posts (id) on delete cascade,
  user_id uuid references auth.users (id) on delete cascade,
  created_at timestamptz default now(),
  primary key (post_id, user_id)
);

-- view
create view public.post_stats as
select p.id, p.title, p.author_id, count(c.id)::int as comment_count, max(c.created_at) as last_comment_at
from public.posts p left join public.comments c on c.post_id = p.id
group by p.id;

-- classic Supabase trigger: create profile on signup
create function public.handle_new_user() returns trigger
language plpgsql security definer set search_path = '' as $$
begin
  insert into public.profiles (id, full_name, avatar_url)
  values (new.id, new.raw_user_meta_data ->> 'full_name', new.raw_user_meta_data ->> 'avatar_url');
  return new;
end; $$;
create trigger on_auth_user_created after insert on auth.users
  for each row execute procedure public.handle_new_user();

-- RPC function
create function public.get_top_posts(lim int default 5)
returns setof public.posts language sql stable as $$
  select * from public.posts where status = 'published' order by views desc limit lim;
$$;

-- RLS
alter table public.profiles enable row level security;
alter table public.posts enable row level security;
alter table public.comments enable row level security;
alter table public.post_likes enable row level security;

create policy "Public profiles are viewable by everyone." on public.profiles for select using (true);
create policy "Users can insert their own profile." on public.profiles for insert with check ((select auth.uid()) = id);
create policy "Users can update own profile." on public.profiles for update using ((select auth.uid()) = id);

create policy "Published posts are public" on public.posts for select using (status = 'published');
create policy "Authors see own posts" on public.posts for select to authenticated using (auth.uid() = author_id);
create policy "Authors create posts" on public.posts for insert to authenticated with check (auth.uid() = author_id);
create policy "Authors update posts" on public.posts for update to authenticated using (auth.uid() = author_id) with check (auth.uid() = author_id);
create policy "Authors delete posts" on public.posts for delete to authenticated using (auth.uid() = author_id);

create policy "Signed-in users read comments" on public.comments for select to authenticated using (true);
create policy "Users comment as themselves" on public.comments for insert to authenticated with check (auth.uid() = user_id);
create policy "Comment owner or post author can delete" on public.comments for delete to authenticated
  using (auth.uid() = user_id or exists (select 1 from public.posts p where p.id = post_id and p.author_id = auth.uid()));

create policy "Likes are public" on public.post_likes for select using (true);
create policy "Like as yourself" on public.post_likes for insert to authenticated with check (auth.uid() = user_id);
create policy "Unlike own" on public.post_likes for delete to authenticated using (auth.uid() = user_id);

-- A second table keyed by the user id, with a primary key that is not called "id". profiles is merged into
-- users; this one must stay a collection of its own (record id = user id), with nothing lost on either side.
create table public.user_settings (
  user_id uuid primary key references auth.users (id) on delete cascade,
  theme text not null default 'light',
  locale text
);
alter table public.user_settings enable row level security;
create policy "Read own settings" on public.user_settings for select to authenticated using (auth.uid() = user_id);
create policy "Update own settings" on public.user_settings for update to authenticated using (auth.uid() = user_id);

-- Tables app users cannot reach on Supabase must stay locked on PocketBase: a policy for service_role only,
-- a policy for an internal role (the pattern of Supabase's RBAC guide), and a table without privileges.
create table public.internal_jobs (id bigint generated always as identity primary key, payload text);
alter table public.internal_jobs enable row level security;
create policy "Service role full access" on public.internal_jobs for all to service_role using (true) with check (true);
insert into public.internal_jobs (payload) values ('nightly export'), ('invoice run');

create table public.user_roles (
  id bigint generated always as identity primary key,
  user_id uuid references auth.users (id) on delete cascade,
  role text not null
);
alter table public.user_roles enable row level security;
create policy "Allow auth admin to read user roles" on public.user_roles as permissive for select to supabase_auth_admin using (true);

create table public.ledger (id bigint generated always as identity primary key, note text);
alter table public.ledger enable row level security;
create policy "Readable by policy, but nobody has the privilege" on public.ledger for select using (true);
revoke all on public.ledger from anon, authenticated;
insert into public.ledger (note) values ('opening balance');

-- Storage buckets & policies (Supabase storage RLS lives on storage.objects)
insert into storage.buckets (id, name, public, file_size_limit) values ('avatars', 'avatars', true, 5242880), ('documents', 'documents', false, 52428800);
create policy "Avatar upload own folder" on storage.objects for insert to authenticated
  with check (bucket_id = 'avatars' and (storage.foldername(name))[1] = auth.uid()::text);
create policy "Docs upload own folder" on storage.objects for insert to authenticated
  with check (bucket_id = 'documents' and (storage.foldername(name))[1] = auth.uid()::text);
create policy "Docs read own" on storage.objects for select to authenticated
  using (bucket_id = 'documents' and (storage.foldername(name))[1] = auth.uid()::text);

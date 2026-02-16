<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Repository\PrivateMessageRepository;
use App\Entity\User;
use Doctrine\DBAL\Types\Types;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: PrivateMessageRepository::class)]
#[ORM\Table(name: 'zchat_private_message')]
#[ORM\Index(columns: ['sender_id', 'recipient_id', 'created_at'], name: 'idx_zchat_pm_conversation')]
#[ORM\Index(columns: ['recipient_id', 'is_read'], name: 'idx_zchat_pm_unread')]
class PrivateMessage
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $sender;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $recipient;

    #[ORM\Column(type: Types::TEXT)]
    private string $content;

    #[ORM\Column]
    private bool $isRead = false;

    #[ORM\Column(nullable: true)]
    private ?\DateTimeImmutable $readAt = null;

    #[ORM\Column]
    private bool $isDeletedBySender = false;

    #[ORM\Column]
    private bool $isDeletedByRecipient = false;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getSender(): User
    {
        return $this->sender;
    }

    public function setSender(User $sender): static
    {
        $this->sender = $sender;
        return $this;
    }

    public function getRecipient(): User
    {
        return $this->recipient;
    }

    public function setRecipient(User $recipient): static
    {
        $this->recipient = $recipient;
        return $this;
    }

    public function getContent(): string
    {
        return $this->content;
    }

    public function setContent(string $content): static
    {
        $this->content = $content;
        return $this;
    }

    public function isRead(): bool
    {
        return $this->isRead;
    }

    public function markAsRead(): static
    {
        $this->isRead = true;
        $this->readAt = new \DateTimeImmutable();
        return $this;
    }

    public function getReadAt(): ?\DateTimeImmutable
    {
        return $this->readAt;
    }

    public function isDeletedBySender(): bool
    {
        return $this->isDeletedBySender;
    }

    public function deleteForSender(): static
    {
        $this->isDeletedBySender = true;
        return $this;
    }

    public function isDeletedByRecipient(): bool
    {
        return $this->isDeletedByRecipient;
    }

    public function deleteForRecipient(): static
    {
        $this->isDeletedByRecipient = true;
        return $this;
    }

    public function isVisibleTo(User $user): bool
    {
        if ($user->getId()->equals($this->sender->getId())) {
            return !$this->isDeletedBySender;
        }
        if ($user->getId()->equals($this->recipient->getId())) {
            return !$this->isDeletedByRecipient;
        }
        return false;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }
}
